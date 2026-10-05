package monitor

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"serverdash/internal/hostinfo"
	"sort"
	"strconv"
	"strings"
	"time"
)

// scriptOpts selects the optional parts of a collection cycle.
type scriptOpts struct {
	inspect     bool           // restart counts, PIDs, network modes and docker events (once a minute)
	eventsSince int64          // unix seconds (server clock); 0 = the last hour
	netPIDs     map[string]int // container id -> init PID, for exact per-container network counters
}

// Container events worth showing; exec_* (healthcheck probes) are left out on purpose.
const eventFilters = "--filter type=container --filter event=create --filter event=start --filter event=restart --filter event=stop " +
	"--filter event=kill --filter event=die --filter event=oom --filter event=destroy --filter event=pause --filter event=unpause --filter event=health_status"

// A single SSH command per cycle; sections are separated by "@@NAME" lines.
func script(docker string, o scriptOpts) string {
	var b strings.Builder
	b.WriteString(baseScript(docker))
	// Exact block I/O bytes per container from cgroup v2 (systemd or cgroupfs driver); readable without root.
	b.WriteString("echo @@CIO; for f in /sys/fs/cgroup/system.slice/docker-*.scope/io.stat /sys/fs/cgroup/docker/*/io.stat; do [ -r \"$f\" ] && echo \"#$f\" && cat \"$f\"; done\n")
	// CPU throttling per container (cgroup v2, or v1 cpu controller): enforcement periods and throttled periods.
	b.WriteString("echo @@CCPU; for f in /sys/fs/cgroup/system.slice/docker-*.scope/cpu.stat /sys/fs/cgroup/docker/*/cpu.stat /sys/fs/cgroup/cpu,cpuacct/docker/*/cpu.stat /sys/fs/cgroup/cpu/docker/*/cpu.stat; do [ -r \"$f\" ] && echo \"#$f\" && grep -E '^nr_(periods|throttled) ' \"$f\"; done\n")
	if len(o.netPIDs) > 0 {
		ids := make([]string, 0, len(o.netPIDs))
		for id := range o.netPIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		b.WriteString("echo @@CNET\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "echo '#%s'; cat /proc/%d/net/dev 2>/dev/null\n", id, o.netPIDs[id])
		}
	}
	if o.inspect {
		b.WriteString("echo @@INSPECT; " + docker + " inspect --format '{{.Id}} {{.RestartCount}} {{.State.Pid}} {{.HostConfig.NetworkMode}}' $(" + docker + " ps -aq --no-trunc) 2>/dev/null\n")
		since := "$(( $(date +%s) - 3600 ))"
		if o.eventsSince > 0 {
			since = strconv.FormatInt(o.eventsSince, 10)
		}
		b.WriteString("now=$(date +%s); echo @@EVENTSUNTIL; echo $now; echo @@EVENTS; " + docker + " events --since " + since +
			" --until $now " + eventFilters + " --format '{{json .}}' 2>/dev/null\n")
	}
	return b.String()
}

func baseScript(docker string) string {
	return `export LC_ALL=C
echo @@HOST; hostname
echo @@OS; (. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")
echo @@KERNEL; uname -r
echo @@UPTIME; cat /proc/uptime
echo @@LOAD; cat /proc/loadavg
echo @@CPUMODEL; grep -m1 'model name' /proc/cpuinfo | cut -d: -f2-
echo @@STAT; grep '^cpu' /proc/stat
echo @@MEM; grep -E '^(MemTotal|MemFree|MemAvailable|Buffers|Cached|SReclaimable|Shmem|SwapTotal|SwapFree):' /proc/meminfo
echo @@VMSTAT; grep -E '^(pswpin|pswpout|oom_kill) ' /proc/vmstat
echo @@DISK; df -PB1 -x tmpfs -x devtmpfs -x overlay -x squashfs -x efivarfs 2>/dev/null
echo @@INODES; df -Pi -x tmpfs -x devtmpfs -x overlay -x squashfs -x efivarfs 2>/dev/null
echo @@DISKSTATS; cat /proc/diskstats
echo @@NET; cat /proc/net/dev
echo @@PSI; for r in cpu memory io; do [ -r /proc/pressure/$r ] && sed "s/^/$r /" /proc/pressure/$r; done
echo @@SOCKSTAT; cat /proc/net/sockstat /proc/net/sockstat6 2>/dev/null
echo @@DOCKER_PS; ` + docker + ` ps -a --no-trunc --format '{{json .}}' 2>&1
echo @@DOCKER_STATS; ` + docker + ` stats --no-stream --no-trunc --format '{{json .}}' 2>&1
`
}

type Disk struct {
	FS       string   `json:"fs"`
	Mount    string   `json:"mount"`
	Size     float64  `json:"size"`
	Used     float64  `json:"used"`
	Pct      float64  `json:"pct"`
	InodePct *float64 `json:"inodePct"` // nil when the filesystem has no fixed inode table (btrfs, vfat…)
}

type Mem struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Pct       float64 `json:"pct"`
	Cache     float64 `json:"cache,omitempty"`     // buffers + page cache + reclaimable slab
	Available float64 `json:"available,omitempty"` // what applications can still get
}

type Net struct {
	RX *float64 `json:"rx"`
	TX *float64 `json:"tx"`
}

// CPUSplit is the share of CPU time per category, in %.
type CPUSplit struct {
	User   float64 `json:"user"`   // user + nice
	System float64 `json:"system"` // system + irq + softirq
	IOWait float64 `json:"iowait"` // idle while waiting for disk I/O
	Steal  float64 `json:"steal"`  // taken by the hypervisor for other VMs
}

// DiskIO is the activity of one physical block device.
type DiskIO struct {
	Name     string  `json:"name"`
	ReadBps  float64 `json:"readBps"`
	WriteBps float64 `json:"writeBps"`
	IOPS     float64 `json:"iops"`
	UtilPct  float64 `json:"util"`
	AwaitMs  float64 `json:"await"` // average time per read/write, queue included
}

// PSI is the share of time (%) tasks were stalled waiting for a resource (Linux pressure stall information).
// "Some" = at least one task waited; "full" = all non-idle tasks waited at once (memory and I/O only).
type PSI struct {
	CPU     *float64 `json:"cpu"`
	Mem     *float64 `json:"mem"`
	MemFull *float64 `json:"memFull"`
	IO      *float64 `json:"io"`
	IOFull  *float64 `json:"ioFull"`
}

// TCP sockets from /proc/net/sockstat (IPv4 + IPv6).
type TCP struct {
	InUse    float64 `json:"inUse"`    // open TCP sockets (established and other states)
	TimeWait float64 `json:"timeWait"` // closed, waiting for late packets
}

type Host struct {
	Hostname string     `json:"hostname"`
	OS       string     `json:"os"`
	Kernel   string     `json:"kernel"`
	Uptime   float64    `json:"uptime"`
	Load     [3]float64 `json:"load"`
	CPUModel string     `json:"cpuModel"`
	Cores    int        `json:"cores"`
	CPU      *float64   `json:"cpu"`
	CPUSplit *CPUSplit  `json:"cpuSplit"`
	PerCore  []*float64 `json:"perCore"`
	Mem      Mem        `json:"mem"`
	Swap     Mem        `json:"swap"`
	SwapIn   *float64   `json:"swapIn"`   // bytes/s
	SwapOut  *float64   `json:"swapOut"`  // bytes/s
	OOMKills float64    `json:"oomKills"` // since boot
	Disks    []Disk     `json:"disks"`
	DiskIO   []DiskIO   `json:"diskIO"`
	IORead   *float64   `json:"ioRead"`  // bytes/s, all devices
	IOWrite  *float64   `json:"ioWrite"` // bytes/s, all devices
	IOUtil   *float64   `json:"ioUtil"`  // busiest disk, % of time busy
	IOAwait  *float64   `json:"ioAwait"` // ms per request, all disks
	Net      Net        `json:"net"`
	PSI      *PSI       `json:"psi"` // nil when the kernel has no PSI (< 4.20 or disabled)
	TCP      *TCP       `json:"tcp"`
}

type Container struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Image     string   `json:"image"`
	State     string   `json:"state"`
	Status    string   `json:"status"`
	Ports     string   `json:"ports"`
	CreatedAt string   `json:"createdAt"`
	Project   string   `json:"project"`
	Service   string   `json:"service"`
	CPU       *float64 `json:"cpu"`
	MemUsed   *float64 `json:"memUsed"`
	MemLimit  *float64 `json:"memLimit"`
	MemPct    *float64 `json:"memPct"`
	// MemLimitPct is the usage against a real memory limit (nil when the container has none).
	MemLimitPct *float64 `json:"memLimitPct"`
	// Throttled is the share of CPU enforcement periods in which the container hit its CPU limit (%).
	Throttled *float64 `json:"throttled"`
	NetIO     string   `json:"netIO"`
	BlockIO   string   `json:"blockIO"`
	PIDs      *int     `json:"pids"`
	// Health is healthy, unhealthy or starting; empty when the image has no healthcheck.
	Health       string `json:"health,omitempty"`
	ExitCode     *int   `json:"exitCode"`          // for exited containers
	RestartCount *int   `json:"restartCount"`      // refreshed once a minute
	Looping      bool   `json:"looping,omitempty"` // restarted 3+ times in the last 10 minutes
	// Rates in bytes/s; nil until two samples exist (or for host-network containers).
	NetRx    *float64    `json:"netRx"`
	NetTx    *float64    `json:"netTx"`
	BlkRead  *float64    `json:"blkRead"`
	BlkWrite *float64    `json:"blkWrite"`
	counters ctrCounters // cumulative values behind the rates (not sent to the UI)
}

// ctrCounters are cumulative bytes. Exact sources ('p' /proc, 'c' cgroup) are preferred over
// the rounded strings of docker stats ('s'); rates are only computed between samples of the same source.
type ctrCounters struct {
	net, blk       [2]float64
	netSrc, blkSrc byte
	cpu            [2]float64 // cgroup nr_periods, nr_throttled
	hasCPU         bool
}

// Event is a container lifecycle event from `docker events`.
type Event struct {
	T         int64  `json:"t"` // unix ms
	Container string `json:"container"`
	Action    string `json:"action"`           // create, start, restart, stop, kill, die, oom, destroy, pause, unpause, health
	Detail    string `json:"detail,omitempty"` // exit code, signal or health status
}

// inspectInfo is refreshed once a minute.
type inspectInfo struct {
	restarts int
	pid      int
	netMode  string
}

type Docker struct {
	Available  bool        `json:"available"`
	Error      string      `json:"error,omitempty"`
	Running    int         `json:"running"`
	Unhealthy  int         `json:"unhealthy"`
	Total      int         `json:"total"`
	Containers []Container `json:"containers"`
}

type Snapshot struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // online | offline | connecting | rebooting
	// RebootingSince is when a reboot was requested from the app (status "rebooting").
	RebootingSince int64         `json:"rebootingSince,omitempty"`
	Error          string        `json:"error,omitempty"`
	UpdatedAt      int64         `json:"updatedAt"`
	Host           *Host         `json:"host"`
	Docker         *Docker       `json:"docker"`
	Alerts         []ActiveAlert `json:"alerts"`
	// Maintenance is refreshed hourly (reboot needed, pending updates).
	Maintenance *hostinfo.Maintenance `json:"maintenance"`
}

type cpuTimes struct{ idle, total, user, system, iowait, steal float64 }

type ioCounters struct{ ops, rsect, wsect, ticks, qticks float64 }

// raw keeps the cumulative counters of the previous sample to compute rates.
type raw struct {
	at     time.Time
	cpu    map[string]cpuTimes
	rx, tx float64
	disk   map[string]ioCounters
	vm     map[string]float64
	psi    map[string]float64 // "cpu some" -> total stalled µs
	// inspect is nil when the cycle did not run docker inspect.
	inspect     map[string]inspectInfo
	ctr         map[string]ctrCounters
	events      []Event
	eventsUntil int64 // server clock, unix seconds
}

func sections(text string) map[string][]string {
	out := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "@@") {
			cur = strings.TrimSpace(line[2:])
			out[cur] = nil
			continue
		}
		if cur != "" && strings.TrimSpace(line) != "" {
			out[cur] = append(out[cur], line)
		}
	}
	return out
}

func first(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func ptr[T any](v T) *T { return &v }

func round(v float64, digits int) float64 {
	p := math.Pow10(digits)
	return math.Round(v*p) / p
}

func parseCPU(lines []string) (map[string]cpuTimes, []string) {
	res := map[string]cpuTimes{}
	var cores []string
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		var total float64
		// Only sum the first 8 columns: guest/guest_nice are already included in user/nice.
		for i := 1; i < len(f) && i <= 8; i++ {
			total += num(f[i])
		}
		col := func(i int) float64 {
			if i < len(f) {
				return num(f[i])
			}
			return 0
		}
		// Columns: user nice system idle iowait irq softirq steal.
		res[f[0]] = cpuTimes{
			idle: col(4) + col(5), total: total,
			user: col(1) + col(2), system: col(3) + col(6) + col(7), iowait: col(5), steal: col(8),
		}
		if f[0] != "cpu" {
			cores = append(cores, f[0])
		}
	}
	return res, cores
}

func cpuPct(prev, cur cpuTimes, ok bool) *float64 {
	if !ok {
		return nil
	}
	dt := cur.total - prev.total
	if dt <= 0 {
		return ptr(0.0)
	}
	return ptr(round(math.Max(0, math.Min(100, (1-(cur.idle-prev.idle)/dt)*100)), 2))
}

func cpuSplit(prev, cur cpuTimes) *CPUSplit {
	dt := cur.total - prev.total
	if dt <= 0 {
		return &CPUSplit{}
	}
	pct := func(a, b float64) float64 { return round(math.Max(0, (b-a)/dt*100), 2) }
	return &CPUSplit{
		User: pct(prev.user, cur.user), System: pct(prev.system, cur.system),
		IOWait: pct(prev.iowait, cur.iowait), Steal: pct(prev.steal, cur.steal),
	}
}

// Whole physical disks only (no partitions, loop or device-mapper devices) so traffic is not counted twice.
var diskDev = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

func parseDiskstats(lines []string) map[string]ioCounters {
	out := map[string]ioCounters{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 14 || !diskDev.MatchString(f[2]) {
			continue
		}
		// reads completed, sectors read, writes completed, sectors written, ms spent doing I/O
		out[f[2]] = ioCounters{ops: num(f[3]) + num(f[7]), rsect: num(f[5]), wsect: num(f[9]), ticks: num(f[12]), qticks: num(f[6]) + num(f[10])}
	}
	return out
}

func parseKV(lines []string, sep string) map[string]float64 {
	out := map[string]float64{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, sep)
		if ok {
			out[strings.TrimSpace(k)] = num(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
		}
	}
	return out
}

var ignoredIface = regexp.MustCompile(`^(lo|veth|docker|br-|virbr|cni|flannel|cali|tun|tap)`)

func parseNet(lines []string) (rx, tx float64) {
	for i, l := range lines {
		if i < 2 {
			continue
		}
		name, rest, ok := strings.Cut(l, ":")
		if !ok || ignoredIface.MatchString(strings.TrimSpace(name)) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) >= 9 {
			rx += num(f[0])
			tx += num(f[8])
		}
	}
	return
}

var sizeRe = regexp.MustCompile(`(?i)([\d.]+)\s*([a-z]*)`)
var units = map[string]float64{
	"b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
	"kib": 1024, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
}

func parseSize(s string) float64 {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	mult, ok := units[strings.ToLower(m[2])]
	if !ok {
		mult = 1
	}
	return num(m[1]) * mult
}

type psRow struct {
	ID, Names, Image, State, Status, Ports, CreatedAt, Labels string
}

type statsRow struct {
	ID, CPUPerc, MemUsage, MemPerc, NetIO, BlockIO, PIDs string
}

var (
	healthRe = regexp.MustCompile(`\((healthy|unhealthy|health: starting)\)`)
	exitRe   = regexp.MustCompile(`^Exited \((-?\d+)\)`)
)

func parseInspect(lines []string) map[string]inspectInfo {
	out := map[string]inspectInfo{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		info := inspectInfo{restarts: n}
		if len(f) >= 4 {
			info.pid, _ = strconv.Atoi(f[2])
			info.netMode = f[3]
		}
		out[f[0]] = info
	}
	return out
}

var containerIDRe = regexp.MustCompile(`[0-9a-f]{64}`)

// parseCgroupIO sums rbytes/wbytes of every device in each container's io.stat.
func parseCgroupIO(lines []string) map[string][2]float64 {
	out := map[string][2]float64{}
	id := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			id = containerIDRe.FindString(l)
			continue
		}
		if id == "" {
			continue
		}
		v := out[id]
		for _, kv := range strings.Fields(l) {
			k, val, _ := strings.Cut(kv, "=")
			switch k {
			case "rbytes":
				v[0] += num(val)
			case "wbytes":
				v[1] += num(val)
			}
		}
		out[id] = v
	}
	return out
}

// parseCgroupCPU reads nr_periods / nr_throttled of each container's cgroup.
func parseCgroupCPU(lines []string) map[string][2]float64 {
	out := map[string][2]float64{}
	id := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			id = containerIDRe.FindString(l)
			continue
		}
		f := strings.Fields(l)
		if id == "" || len(f) != 2 {
			continue
		}
		v := out[id]
		switch f[0] {
		case "nr_periods":
			v[0] = num(f[1])
		case "nr_throttled":
			v[1] = num(f[1])
		}
		out[id] = v
	}
	return out
}

// parsePSI reads "cpu some avg10=0.00 avg60=0.00 avg300=0.00 total=123" lines into "cpu some" -> total µs.
func parsePSI(lines []string) map[string]float64 {
	out := map[string]float64{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		for _, kv := range f[2:] {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				out[f[0]+" "+f[1]] = num(v)
			}
		}
	}
	return out
}

// psiRates turns the stalled-time counters into the % of the last interval spent stalled.
func psiRates(prev, cur map[string]float64, dt float64) *PSI {
	if len(cur) == 0 || dt <= 0 {
		return nil
	}
	pct := func(k string) *float64 {
		c, ok1 := cur[k]
		p, ok2 := prev[k]
		if !ok1 || !ok2 || c < p {
			return nil
		}
		return ptr(round(math.Min(100, (c-p)/(dt*1e6)*100), 2))
	}
	return &PSI{CPU: pct("cpu some"), Mem: pct("memory some"), MemFull: pct("memory full"), IO: pct("io some"), IOFull: pct("io full")}
}

// parseSockstat reads "TCP: inuse 5 orphan 0 tw 2 alloc 7 mem 1" and "TCP6: inuse 3".
func parseSockstat(lines []string) *TCP {
	var t TCP
	found := false
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 3 || (f[0] != "TCP:" && f[0] != "TCP6:") {
			continue
		}
		found = true
		for i := 1; i+1 < len(f); i += 2 {
			switch f[i] {
			case "inuse":
				t.InUse += num(f[i+1])
			case "tw":
				t.TimeWait += num(f[i+1])
			}
		}
	}
	if !found {
		return nil
	}
	return &t
}

// parseContainerNet sums the non-loopback interfaces of each container's network namespace.
func parseContainerNet(lines []string) map[string][2]float64 {
	out := map[string][2]float64{}
	id := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			id = strings.TrimPrefix(strings.TrimSpace(l), "#")
			continue
		}
		name, rest, ok := strings.Cut(l, ":")
		if id == "" || !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		v := out[id]
		v[0] += num(f[0])
		v[1] += num(f[8])
		out[id] = v
	}
	return out
}

func parseEvents(lines []string) []Event {
	var out []Event
	for _, l := range lines {
		var e struct {
			Action   string
			Time     int64 `json:"time"`
			TimeNano int64 `json:"timeNano"`
			Actor    struct{ Attributes map[string]string }
		}
		if json.Unmarshal([]byte(l), &e) != nil || e.Action == "" {
			continue
		}
		ev := Event{T: e.Time * 1000, Container: e.Actor.Attributes["name"], Action: e.Action}
		if e.TimeNano > 0 {
			ev.T = e.TimeNano / 1e6
		}
		switch {
		case strings.HasPrefix(e.Action, "health_status"):
			ev.Action = "health"
			ev.Detail = strings.TrimSpace(strings.TrimPrefix(e.Action, "health_status:"))
		case e.Action == "die":
			ev.Detail = e.Actor.Attributes["exitCode"]
		case e.Action == "kill":
			ev.Detail = e.Actor.Attributes["signal"]
		}
		if ev.Container != "" {
			out = append(out, ev)
		}
	}
	return out
}

func parseDocker(psLines, statsLines []string) *Docker {
	d := &Docker{Available: true, Containers: []Container{}}
	var psErr []string
	stats := map[string]statsRow{}
	for _, l := range statsLines {
		var r statsRow
		if json.Unmarshal([]byte(l), &r) == nil {
			stats[r.ID] = r
		}
	}
	for _, l := range psLines {
		var r psRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			psErr = append(psErr, l)
			continue
		}
		c := Container{
			ID: r.ID, Name: r.Names, Image: r.Image, State: r.State, Status: r.Status,
			Ports: r.Ports, CreatedAt: r.CreatedAt,
		}
		if c.State == "" {
			c.State = "exited"
			if strings.HasPrefix(r.Status, "Up") {
				c.State = "running"
			}
		}
		for _, kv := range strings.Split(r.Labels, ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "com.docker.compose.project":
				c.Project = v
			case "com.docker.compose.service":
				c.Service = v
			}
		}
		if m := healthRe.FindStringSubmatch(r.Status); m != nil {
			c.Health = strings.TrimPrefix(m[1], "health: ")
		}
		if m := exitRe.FindStringSubmatch(r.Status); m != nil {
			code, _ := strconv.Atoi(m[1])
			c.ExitCode = &code
		}
		if st, ok := stats[r.ID]; ok && c.State == "running" {
			used, limit, _ := strings.Cut(st.MemUsage, "/")
			c.CPU = ptr(num(strings.TrimSuffix(st.CPUPerc, "%")))
			c.MemUsed = ptr(parseSize(used))
			c.MemLimit = ptr(parseSize(limit))
			c.MemPct = ptr(num(strings.TrimSuffix(st.MemPerc, "%")))
			c.NetIO, c.BlockIO = st.NetIO, st.BlockIO
			rx, tx, _ := strings.Cut(st.NetIO, "/")
			rd, wr, _ := strings.Cut(st.BlockIO, "/")
			c.counters = ctrCounters{net: [2]float64{parseSize(rx), parseSize(tx)}, blk: [2]float64{parseSize(rd), parseSize(wr)}, netSrc: 's', blkSrc: 's'}
			pids, _ := strconv.Atoi(st.PIDs)
			c.PIDs = &pids
		}
		if c.State == "running" {
			d.Running++
		}
		if c.Health == "unhealthy" {
			d.Unhealthy++
		}
		d.Containers = append(d.Containers, c)
	}
	d.Total = len(d.Containers)
	if d.Total == 0 && len(psErr) > 0 {
		d.Available = false
		d.Error = strings.Join(psErr, "\n")
	}
	// Compose projects first (A→Z), standalone containers last.
	sort.SliceStable(d.Containers, func(i, j int) bool {
		a, b := d.Containers[i], d.Containers[j]
		if (a.Project == "") != (b.Project == "") {
			return a.Project != ""
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Name < b.Name
	})
	return d
}

func parse(id, name, out string, prev *raw) (*Snapshot, *raw) {
	s := sections(out)
	now := time.Now()
	stat, cores := parseCPU(s["STAT"])
	cur := &raw{at: now, cpu: stat, disk: parseDiskstats(s["DISKSTATS"]), vm: parseKV(s["VMSTAT"], " "), psi: parsePSI(s["PSI"])}
	if _, ok := s["INSPECT"]; ok {
		cur.inspect = parseInspect(s["INSPECT"])
		cur.events = parseEvents(s["EVENTS"])
		cur.eventsUntil, _ = strconv.ParseInt(first(s["EVENTSUNTIL"]), 10, 64)
	}
	cur.rx, cur.tx = parseNet(s["NET"])

	mem := parseKV(s["MEM"], ":")
	for k := range mem {
		mem[k] *= 1024 // /proc/meminfo is in kB
	}
	h := &Host{
		Hostname: first(s["HOST"]),
		OS:       first(s["OS"]),
		Kernel:   first(s["KERNEL"]),
		CPUModel: first(s["CPUMODEL"]),
		Cores:    len(cores),
		Disks:    []Disk{},
		DiskIO:   []DiskIO{},
		PerCore:  make([]*float64, len(cores)),
		OOMKills: cur.vm["oom_kill"],
	}
	if f := strings.Fields(first(s["UPTIME"])); len(f) > 0 {
		h.Uptime = num(f[0])
	}
	if f := strings.Fields(first(s["LOAD"])); len(f) >= 3 {
		h.Load = [3]float64{num(f[0]), num(f[1]), num(f[2])}
	}
	if prev != nil {
		p, ok := prev.cpu["cpu"]
		h.CPU = cpuPct(p, stat["cpu"], ok)
		if ok {
			h.CPUSplit = cpuSplit(p, stat["cpu"])
		}
		for i, c := range cores {
			p, ok := prev.cpu[c]
			h.PerCore[i] = cpuPct(p, stat[c], ok)
		}
		if dt := now.Sub(prev.at).Seconds(); dt > 0 {
			rate := func(a, b float64) *float64 { return ptr(round(math.Max(0, (b-a)/dt), 0)) }
			h.Net.RX = rate(prev.rx, cur.rx)
			h.Net.TX = rate(prev.tx, cur.tx)
			const page = 4096
			h.SwapIn = rate(prev.vm["pswpin"]*page, cur.vm["pswpin"]*page)
			h.SwapOut = rate(prev.vm["pswpout"]*page, cur.vm["pswpout"]*page)
			var rd, wr, util, qt, nops float64
			for name, c := range cur.disk {
				p, ok := prev.disk[name]
				if !ok {
					continue
				}
				io := DiskIO{
					Name:     name,
					ReadBps:  *rate(p.rsect*512, c.rsect*512),
					WriteBps: *rate(p.wsect*512, c.wsect*512),
					IOPS:     round(math.Max(0, (c.ops-p.ops)/dt), 1),
					UtilPct:  round(math.Min(100, math.Max(0, (c.ticks-p.ticks)/(dt*1000)*100)), 1),
				}
				if ops := c.ops - p.ops; ops > 0 {
					io.AwaitMs = round(math.Max(0, (c.qticks-p.qticks)/ops), 2)
					qt += c.qticks - p.qticks
					nops += ops
				}
				util = math.Max(util, io.UtilPct)
				rd += io.ReadBps
				wr += io.WriteBps
				h.DiskIO = append(h.DiskIO, io)
			}
			sort.Slice(h.DiskIO, func(i, j int) bool { return h.DiskIO[i].Name < h.DiskIO[j].Name })
			h.IORead, h.IOWrite = ptr(rd), ptr(wr)
			if len(h.DiskIO) > 0 {
				h.IOUtil = ptr(util)
				h.IOAwait = ptr(0.0)
				if nops > 0 {
					h.IOAwait = ptr(round(math.Max(0, qt/nops), 2))
				}
			}
			h.PSI = psiRates(prev.psi, cur.psi, dt)
		}
	}
	h.Mem.Total = mem["MemTotal"]
	h.Mem.Available = mem["MemAvailable"]
	h.Mem.Used = mem["MemTotal"] - mem["MemAvailable"]
	h.Mem.Cache = mem["Buffers"] + mem["Cached"] + mem["SReclaimable"] - mem["Shmem"]
	if h.Mem.Cache < 0 {
		h.Mem.Cache = 0
	}
	if h.Mem.Total > 0 {
		h.Mem.Pct = round(h.Mem.Used/h.Mem.Total*100, 2)
	}
	h.Swap.Total = mem["SwapTotal"]
	h.Swap.Used = mem["SwapTotal"] - mem["SwapFree"]

	inodes := map[string]*float64{}
	for i, l := range s["INODES"] {
		f := strings.Fields(l)
		if i == 0 || len(f) < 6 {
			continue
		}
		if total := num(f[1]); total > 0 {
			inodes[strings.Join(f[5:], " ")] = ptr(round(num(f[2])/total*100, 2))
		}
	}
	for i, l := range s["DISK"] {
		f := strings.Fields(l)
		if i == 0 || len(f) < 6 {
			continue
		}
		size, used := num(f[1]), num(f[2])
		mount := strings.Join(f[5:], " ")
		if size <= 0 || strings.HasPrefix(mount, "/boot/efi") {
			continue
		}
		h.Disks = append(h.Disks, Disk{FS: f[0], Mount: mount, Size: size, Used: used, Pct: round(used/size*100, 2), InodePct: inodes[mount]})
	}

	h.TCP = parseSockstat(s["SOCKSTAT"])

	d := parseDocker(s["DOCKER_PS"], s["DOCKER_STATS"])
	containerRates(d, cur, prev, parseCgroupIO(s["CIO"]), parseContainerNet(s["CNET"]), parseCgroupCPU(s["CCPU"]))
	// Memory against a real limit only: without one Docker reports the host's RAM as the limit.
	for i := range d.Containers {
		c := &d.Containers[i]
		if c.MemUsed != nil && c.MemLimit != nil && *c.MemLimit > 0 && (h.Mem.Total == 0 || *c.MemLimit < h.Mem.Total*0.98) {
			c.MemLimitPct = ptr(round(*c.MemUsed / *c.MemLimit * 100, 2))
		}
	}
	return &Snapshot{
		ID: id, Name: name, Status: "online", UpdatedAt: now.UnixMilli(),
		Host: h, Docker: d,
	}, cur
}

// containerRates prefers exact counters and turns them into per-second rates.
func containerRates(d *Docker, cur, prev *raw, cio, cnet, ccpu map[string][2]float64) {
	cur.ctr = map[string]ctrCounters{}
	var dt float64
	if prev != nil {
		dt = cur.at.Sub(prev.at).Seconds()
	}
	rate := func(a, b float64) *float64 {
		if b < a || dt <= 0 {
			return nil // counters reset (container restarted)
		}
		return ptr(round((b-a)/dt, 0))
	}
	for i := range d.Containers {
		c := &d.Containers[i]
		if c.State != "running" {
			continue
		}
		cnt := c.counters
		if v, ok := cio[c.ID]; ok {
			cnt.blk, cnt.blkSrc = v, 'c'
		}
		if v, ok := cnet[c.ID]; ok {
			cnt.net, cnt.netSrc = v, 'p'
		}
		if v, ok := ccpu[c.ID]; ok {
			cnt.cpu, cnt.hasCPU = v, true
		}
		c.counters = cnt
		cur.ctr[c.ID] = cnt
		if prev == nil {
			continue
		}
		p, ok := prev.ctr[c.ID]
		if !ok {
			continue
		}
		if cnt.netSrc != 0 && cnt.netSrc == p.netSrc {
			c.NetRx, c.NetTx = rate(p.net[0], cnt.net[0]), rate(p.net[1], cnt.net[1])
		}
		if cnt.blkSrc != 0 && cnt.blkSrc == p.blkSrc {
			c.BlkRead, c.BlkWrite = rate(p.blk[0], cnt.blk[0]), rate(p.blk[1], cnt.blk[1])
		}
		// Periods only advance when the container has a CPU limit: no periods, no throttling figure.
		if cnt.hasCPU && p.hasCPU {
			if periods := cnt.cpu[0] - p.cpu[0]; periods > 0 {
				c.Throttled = ptr(round(math.Min(100, math.Max(0, (cnt.cpu[1]-p.cpu[1])/periods*100)), 2))
			}
		}
	}
}
