// Package monitor periodically collects metrics from every server, keeps short-term history in memory
// and detects incidents (server unreachable, container stopped unexpectedly, thresholds exceeded).
package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"serverdash/internal/hostinfo"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

const (
	activeInterval = 5 * time.Second  // window visible
	idleInterval   = 30 * time.Second // window minimized/hidden
	historyMax     = 360              // 30 minutes at 5s cycles
	collectTimeout = 25 * time.Second
	hysteresis     = 5.0 // a threshold alert resolves only once the value drops this many points below it
	maintEvery     = time.Hour
	restartsEvery  = time.Minute
	loopWindow     = 10 * time.Minute
	loopRestarts   = 3 // restarts within loopWindow that count as a restart loop
	maintRetry     = 10 * time.Minute
	rebootTimeout  = 15 * time.Minute // a rebooting server that is not back by then raises an alert
)

type Point struct {
	T        int64    `json:"t"`
	CPU      *float64 `json:"cpu"`
	Mem      float64  `json:"mem"`
	RX       *float64 `json:"rx"`
	TX       *float64 `json:"tx"`
	User     *float64 `json:"user"`
	System   *float64 `json:"system"`
	IOWait   *float64 `json:"iowait"`
	Steal    *float64 `json:"steal"`
	Load1    float64  `json:"load1"`
	Load5    float64  `json:"load5"`
	Load15   float64  `json:"load15"`
	MemUsed  float64  `json:"memUsed"`
	MemCache float64  `json:"memCache"`
	IORead   *float64 `json:"ioRead"`
	IOWrite  *float64 `json:"ioWrite"`
}

// PointOf extracts the chart values of a snapshot.
func PointOf(s *Snapshot) Point {
	h := s.Host
	p := Point{
		T: s.UpdatedAt, CPU: h.CPU, Mem: h.Mem.Pct, RX: h.Net.RX, TX: h.Net.TX,
		Load1: h.Load[0], Load5: h.Load[1], Load15: h.Load[2],
		MemUsed: h.Mem.Used, MemCache: h.Mem.Cache, IORead: h.IORead, IOWrite: h.IOWrite,
	}
	if c := h.CPUSplit; c != nil {
		p.User, p.System, p.IOWait, p.Steal = &c.User, &c.System, &c.IOWait, &c.Steal
	}
	return p
}

// Alert is a notification for the UI and Windows. Critical alerts are shown as errors.
type Alert struct {
	Title    string
	Body     string
	Critical bool
}

// ActiveAlert is a threshold alert that is currently firing.
type ActiveAlert struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Since  int64  `json:"since"`
}

type alertState struct {
	since  time.Time
	firing bool
}

type Monitor struct {
	st   *store.Store
	pool *sshpool.Pool

	OnSnapshot func(*Snapshot)
	OnRemoved  func(id string)
	OnAlert    func(Alert)
	OnEvents   func(serverID string, events []Event)

	mu       sync.Mutex
	snaps    map[string]*Snapshot
	prev     map[string]*raw
	history  map[string][]Point
	inFlight map[string]bool
	fails    map[string]int
	expected map[string]time.Time              // containers stopped by the user -> no alert
	alerts   map[string]map[string]*alertState // server -> alert key -> state
	maint    map[string]*hostinfo.Maintenance
	maintAt  map[string]time.Time // last attempt
	maintRun map[string]bool
	reboots  map[string]*rebootState // servers rebooted from the app, until they are up again
	certWarn map[string]bool         // server/cert path -> expiry already announced

	restarts   map[string]map[string]int         // server -> container id -> restart count
	restartsAt map[string]time.Time              // last time restart counts were fetched
	restartLog map[string]map[string][]time.Time // server -> container name -> recent restart times
	inspect    map[string]map[string]inspectInfo // server -> container id -> PID / network mode
	evSince    map[string]int64                  // server -> next docker events --since (server clock)

	visible bool
	wake    chan struct{}
}

func New(st *store.Store, pool *sshpool.Pool) *Monitor {
	m := &Monitor{
		st: st, pool: pool, visible: true, wake: make(chan struct{}, 1),
		snaps: map[string]*Snapshot{}, prev: map[string]*raw{}, history: map[string][]Point{},
		inFlight: map[string]bool{}, fails: map[string]int{}, expected: map[string]time.Time{},
		alerts: map[string]map[string]*alertState{},
		maint:  map[string]*hostinfo.Maintenance{}, maintAt: map[string]time.Time{}, maintRun: map[string]bool{}, reboots: map[string]*rebootState{}, certWarn: map[string]bool{},
		restarts: map[string]map[string]int{}, restartsAt: map[string]time.Time{}, restartLog: map[string]map[string][]time.Time{},
		inspect: map[string]map[string]inspectInfo{}, evSince: map[string]int64{},
	}
	st.OnChange(m.onServerChange)
	return m
}

func (m *Monitor) onServerChange(id string) {
	m.mu.Lock()
	delete(m.prev, id)
	_, exists := m.st.Get(id)
	if !exists {
		delete(m.snaps, id)
		delete(m.history, id)
		delete(m.fails, id)
		delete(m.alerts, id)
		delete(m.maint, id)
		delete(m.maintAt, id)
		delete(m.restarts, id)
		delete(m.restartsAt, id)
		delete(m.restartLog, id)
		delete(m.inspect, id)
		delete(m.evSince, id)
	}
	m.mu.Unlock()
	if !exists {
		m.OnRemoved(id)
		return
	}
	go m.Collect(id)
}

// Run loops until ctx is cancelled. Each server is collected independently,
// so a slow or hung server never delays the others.
func (m *Monitor) Run(ctx context.Context) {
	for {
		for _, s := range m.st.List() {
			go m.Collect(s.ID)
		}
		m.mu.Lock()
		interval := idleInterval
		if m.visible {
			interval = activeInterval
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-m.wake:
		}
	}
}

// SetVisible lowers the polling rate while the window is hidden to save resources.
func (m *Monitor) SetVisible(v bool) {
	m.mu.Lock()
	wasHidden := !m.visible
	m.visible = v
	m.mu.Unlock()
	if v && wasHidden {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

// Expect marks a container that the user is about to stop/restart.
func (m *Monitor) Expect(serverID, container string) {
	m.mu.Lock()
	m.expected[serverID+"/"+container] = time.Now()
	m.mu.Unlock()
}

type rebootState struct {
	at      time.Time
	running []string // containers running when the reboot was requested
}

// ExpectReboot marks a server that the user is rebooting: it shows as "rebooting" instead of offline,
// raises no unreachable/stopped-container alerts, and reports once it is up again.
func (m *Monitor) ExpectReboot(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := &rebootState{at: time.Now()}
	if s := m.snaps[id]; s != nil && s.Docker != nil {
		for _, c := range s.Docker.Containers {
			if c.State == "running" {
				st.running = append(st.running, c.Name)
			}
		}
	}
	m.reboots[id] = st
}

// CancelReboot forgets a reboot that could not be started.
func (m *Monitor) CancelReboot(id string) {
	m.mu.Lock()
	delete(m.reboots, id)
	m.mu.Unlock()
}

// rebootLocked follows a reboot: the server is back once it answers with an uptime shorter than the time
// since the request. Returns whether the reboot is still in progress, and the alerts to raise.
func (m *Monitor) rebootLocked(srv store.Server, snap *Snapshot, ok bool) (bool, []Alert) {
	st := m.reboots[srv.ID]
	if st == nil {
		return false, nil
	}
	since := time.Since(st.at)
	if ok && snap.Host != nil && snap.Host.Uptime > 0 && snap.Host.Uptime < since.Seconds() {
		delete(m.reboots, srv.ID)
		m.fails[srv.ID] = 0             // no separate "back online" alert
		m.maintAt[srv.ID] = time.Time{} // re-check "reboot required" now
		body := "Back online after " + since.Round(time.Second).String()
		running := map[string]bool{}
		if snap.Docker != nil {
			for _, c := range snap.Docker.Containers {
				running[c.Name] = c.State == "running"
			}
		}
		var down []string
		for _, name := range st.running {
			if !running[name] {
				down = append(down, name)
			}
		}
		if len(down) > 0 {
			return false, []Alert{{"Server rebooted: " + srv.Name, fmt.Sprintf("%s · not running again: %s", body, strings.Join(down, ", ")), true}}
		}
		return false, []Alert{{"Server rebooted: " + srv.Name, body, false}}
	}
	if since > rebootTimeout {
		delete(m.reboots, srv.ID)
		return false, []Alert{{"Server not back after reboot: " + srv.Name, fmt.Sprintf("No answer %d minutes after the reboot", int(rebootTimeout.Minutes())), true}}
	}
	snap.Status = "rebooting"
	snap.RebootingSince = st.at.UnixMilli()
	return true, nil
}

func (m *Monitor) Collect(id string) {
	m.mu.Lock()
	if m.inFlight[id] {
		m.mu.Unlock()
		return
	}
	m.inFlight[id] = true
	prev := m.prev[id]
	opts := scriptOpts{
		inspect:     time.Since(m.restartsAt[id]) >= restartsEvery,
		eventsSince: m.evSince[id],
		netPIDs:     map[string]int{},
	}
	backfill := m.evSince[id] == 0 // the first events fetch covers the last hour: show it, don't alert on it
	// Exact network counters need the container's PID; host-network containers would report the whole host.
	for cid, info := range m.inspect[id] {
		if info.pid > 0 && info.netMode != "host" && !strings.HasPrefix(info.netMode, "container:") {
			opts.netPIDs[cid] = info.pid
		}
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.inFlight, id)
		m.mu.Unlock()
	}()

	srv, ok := m.st.Get(id)
	if !ok {
		return
	}
	var snap *Snapshot
	var cur *raw
	var events []Event
	conn, err := m.pool.Get(id)
	if err == nil {
		var res sshpool.Result
		res, err = conn.Exec(context.Background(), script(sshpool.DockerBin(srv), opts), collectTimeout)
		if err == nil {
			snap, cur = parse(id, srv.Name, res.Stdout, prev)
		}
	}
	m.mu.Lock()
	if _, still := m.st.Get(id); !still {
		m.mu.Unlock()
		return
	}
	last := m.snaps[id]
	if err != nil {
		delete(m.prev, id)
		m.fails[id]++
		snap = &Snapshot{ID: id, Name: srv.Name, Status: "offline", Error: err.Error(), UpdatedAt: time.Now().UnixMilli()}
		if last != nil {
			snap.Host, snap.Docker = last.Host, last.Docker
		}
	} else {
		m.prev[id] = cur
		var fresh map[string]int
		if cur.inspect != nil {
			fresh = map[string]int{}
			for cid, info := range cur.inspect {
				fresh[cid] = info.restarts
			}
			m.inspect[id] = cur.inspect
			if cur.eventsUntil > 0 {
				m.evSince[id] = cur.eventsUntil
			}
			events = cur.events
		}
		m.applyRestartsLocked(id, snap, fresh)
		// Host-network containers share the host's interfaces: per-container network numbers are meaningless.
		if snap.Docker != nil {
			for i := range snap.Docker.Containers {
				c := &snap.Docker.Containers[i]
				if info, ok := m.inspect[id][c.ID]; ok && info.netMode == "host" {
					c.NetRx, c.NetTx = nil, nil
				}
			}
		}
		if snap.Host.CPU != nil {
			h := append(m.history[id], PointOf(snap))
			if len(h) > historyMax {
				h = h[len(h)-historyMax:]
			}
			m.history[id] = h
		}
	}
	rebooting, alerts := m.rebootLocked(srv, snap, err == nil)
	if !rebooting && alerts == nil {
		alerts = m.detectLocked(srv, last, snap)
	}
	if err == nil {
		m.fails[id] = 0
		alerts = append(alerts, m.thresholdsLocked(srv, last, snap)...)
	}
	snap.Maintenance = m.maint[id]
	// Maintenance status changes slowly: check hourly (sooner after a failure) in the background.
	dueMaint := err == nil && !m.maintRun[id] && time.Since(m.maintAt[id]) >= maintEvery
	m.snaps[id] = snap
	m.mu.Unlock()
	if dueMaint {
		go func() { _ = m.CheckMaintenance(id) }()
	}

	if !backfill {
		for _, e := range events {
			if e.Action == "oom" {
				alerts = append(alerts, Alert{"Container out of memory: " + e.Container, srv.Name + " · the kernel killed a process inside it", true})
			}
		}
	}
	if len(events) > 0 && m.OnEvents != nil {
		m.OnEvents(id, events)
	}
	m.OnSnapshot(snap)
	for _, a := range alerts {
		m.OnAlert(a)
	}
}

// applyRestartsLocked attaches restart counts (fresh or cached) to the containers and flags restart loops.
func (m *Monitor) applyRestartsLocked(id string, snap *Snapshot, fresh map[string]int) {
	if snap.Docker == nil {
		return
	}
	now := time.Now()
	old := m.restarts[id]
	if fresh != nil {
		m.restarts[id] = fresh
		m.restartsAt[id] = now
	}
	counts := m.restarts[id]
	log := m.restartLog[id]
	if log == nil {
		log = map[string][]time.Time{}
		m.restartLog[id] = log
	}
	for i := range snap.Docker.Containers {
		c := &snap.Docker.Containers[i]
		n, ok := counts[c.ID]
		if !ok {
			continue
		}
		c.RestartCount = &n
		if fresh != nil && old != nil {
			if before, seen := old[c.ID]; seen && n > before {
				for k := 0; k < n-before && k < loopRestarts; k++ {
					log[c.Name] = append(log[c.Name], now)
				}
			}
		}
		recent := log[c.Name][:0]
		for _, t := range log[c.Name] {
			if now.Sub(t) < loopWindow {
				recent = append(recent, t)
			}
		}
		log[c.Name] = recent
		c.Looping = len(recent) >= loopRestarts
	}
}

func (m *Monitor) detectLocked(srv store.Server, last, cur *Snapshot) []Alert {
	var out []Alert
	// Only report unreachable after 2 consecutive failures to avoid false alarms on flaky networks.
	if cur.Status == "offline" && m.fails[srv.ID] == 2 {
		out = append(out, Alert{"Server unreachable: " + srv.Name, cur.Error, true})
	}
	if cur.Status == "online" && m.fails[srv.ID] >= 2 {
		out = append(out, Alert{"Server back online: " + srv.Name, "The server is responding again", false})
	}
	if last == nil || last.Docker == nil || cur.Docker == nil || cur.Status != "online" {
		return out
	}
	before := map[string]Container{}
	for _, c := range last.Docker.Containers {
		before[c.Name] = c
	}
	for _, c := range cur.Docker.Containers {
		prevC, seen := before[c.Name]
		if seen && c.Health == "unhealthy" && prevC.Health != "unhealthy" {
			out = append(out, Alert{"Container unhealthy: " + c.Name, srv.Name + " · its healthcheck is failing", true})
		}
		if seen && prevC.Health == "unhealthy" && c.Health == "healthy" {
			out = append(out, Alert{"Container healthy again: " + c.Name, srv.Name, false})
		}
		if seen && c.Looping && !prevC.Looping {
			out = append(out, Alert{"Container restarting repeatedly: " + c.Name, fmt.Sprintf("%s · %d+ restarts in the last %d minutes", srv.Name, loopRestarts, int(loopWindow.Minutes())), true})
		}
		if prevC.State != "running" || c.State == "running" {
			continue
		}
		key := srv.ID + "/" + c.Name
		if t, ok := m.expected[key]; ok && time.Since(t) < 2*time.Minute {
			continue
		}
		out = append(out, Alert{fmt.Sprintf("Container stopped: %s", c.Name), fmt.Sprintf("%s · %s", srv.Name, c.Status), true})
	}
	for k, t := range m.expected {
		if time.Since(t) > 2*time.Minute {
			delete(m.expected, k)
		}
	}
	return out
}

// thresholdsLocked evaluates the threshold alerts and records the firing ones on the snapshot.
func (m *Monitor) thresholdsLocked(srv store.Server, last, cur *Snapshot) []Alert {
	h := cur.Host
	th := m.st.Settings().Alerts
	sustain := time.Duration(th.SustainMinutes * float64(time.Minute))
	states := m.alerts[srv.ID]
	if states == nil {
		states = map[string]*alertState{}
		m.alerts[srv.ID] = states
	}
	now := time.Now()
	var out []Alert
	cur.Alerts = []ActiveAlert{}
	seen := map[string]bool{}

	check := func(key, label string, value *float64, limit float64, wait time.Duration) {
		seen[key] = true
		st := states[key]
		if st == nil {
			st = &alertState{}
			states[key] = st
		}
		if value == nil {
			return
		}
		v := *value
		switch {
		case v >= limit:
			if st.since.IsZero() {
				st.since = now
			}
			if !st.firing && now.Sub(st.since) >= wait {
				st.firing = true
				dur := ""
				if wait > 0 {
					dur = fmt.Sprintf(" for %g min", wait.Minutes())
				}
				out = append(out, Alert{fmt.Sprintf("%s: %s", label, srv.Name), fmt.Sprintf("%.0f%%%s (threshold %.0f%%)", v, dur, limit), true})
			}
		case !st.firing:
			st.since = time.Time{}
		case v < limit-hysteresis:
			st.since, st.firing = time.Time{}, false
			out = append(out, Alert{fmt.Sprintf("Resolved: %s on %s", strings.ToLower(label[:1])+label[1:], srv.Name), fmt.Sprintf("Now %.0f%%", v), false})
		}
		if st.firing {
			cur.Alerts = append(cur.Alerts, ActiveAlert{Key: key, Title: label, Detail: fmt.Sprintf("%.0f%%", v), Since: st.since.UnixMilli()})
		}
	}

	check("cpu", "High CPU", h.CPU, th.CPU, sustain)
	check("memory", "High memory", &h.Mem.Pct, th.Memory, sustain)
	if h.CPUSplit != nil {
		check("iowait", "High I/O wait", &h.CPUSplit.IOWait, th.IOWait, sustain)
	} else {
		check("iowait", "High I/O wait", nil, th.IOWait, sustain)
	}
	for _, d := range h.Disks {
		check("disk:"+d.Mount, "Disk "+d.Mount+" almost full", &d.Pct, th.Disk, 0)
		check("inode:"+d.Mount, "Inodes on "+d.Mount+" almost exhausted", d.InodePct, th.Disk, 0)
	}
	// Forget state of mounts that disappeared.
	for key := range states {
		if !seen[key] {
			delete(states, key)
		}
	}
	// The kernel killed processes for lack of memory since the previous sample.
	if last != nil && last.Host != nil && last.Status == "online" && h.OOMKills > last.Host.OOMKills {
		n := h.OOMKills - last.Host.OOMKills
		out = append(out, Alert{"Out of memory: " + srv.Name, fmt.Sprintf("The kernel killed %.0f process(es) to free memory", n), true})
	}
	return out
}

// CheckMaintenance refreshes the reboot / pending updates status of a server now.
func (m *Monitor) CheckMaintenance(id string) error {
	m.mu.Lock()
	if m.maintRun[id] {
		m.mu.Unlock()
		return nil
	}
	m.maintRun[id] = true
	m.maintAt[id] = time.Now()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.maintRun, id)
		m.mu.Unlock()
	}()
	conn, err := m.pool.Get(id)
	var info *hostinfo.Maintenance
	if err == nil {
		info, err = hostinfo.CheckMaintenance(context.Background(), conn)
	}
	m.mu.Lock()
	if err != nil {
		// Retry sooner than the normal hourly cycle.
		m.maintAt[id] = time.Now().Add(maintRetry - maintEvery)
	}
	srv, known := m.st.Get(id)
	var alerts []Alert
	if err == nil && known {
		m.maint[id] = info
		alerts = m.certAlertsLocked(srv, info)
	}
	m.mu.Unlock()
	for _, a := range alerts {
		m.OnAlert(a)
	}
	if err == nil {
		go m.Collect(id) // publish the new status right away
	}
	return err
}

// CertWarnDays: certificates expiring within this many days raise an alert.
const CertWarnDays = 14

func (m *Monitor) certAlertsLocked(srv store.Server, info *hostinfo.Maintenance) []Alert {
	var out []Alert
	now := time.Now()
	for _, c := range info.Certs {
		key := srv.ID + "|" + c.Path
		days := c.DaysLeft(now)
		if days > CertWarnDays {
			delete(m.certWarn, key)
			continue
		}
		if m.certWarn[key] {
			continue
		}
		m.certWarn[key] = true
		name := c.Subject
		if name == "" {
			name = c.Path
		}
		when := fmt.Sprintf("expires in %d days", days)
		if days < 0 {
			when = "has expired"
		}
		out = append(out, Alert{"Certificate " + when + ": " + name, srv.Name + " · " + c.Path, true})
	}
	return out
}

type InitData struct {
	Snapshots []*Snapshot        `json:"snapshots"`
	History   map[string][]Point `json:"history"`
}

func (m *Monitor) All() InitData {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := InitData{Snapshots: []*Snapshot{}, History: map[string][]Point{}}
	for _, s := range m.st.List() {
		snap := m.snaps[s.ID]
		if snap == nil {
			snap = &Snapshot{ID: s.ID, Name: s.Name, Status: "connecting"}
		}
		out.Snapshots = append(out.Snapshots, snap)
		out.History[s.ID] = append([]Point(nil), m.history[s.ID]...)
	}
	return out
}
