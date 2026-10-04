package monitor

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"serverdash/internal/store"
)

// sample2 builds collector output with the given cumulative counters.
func sample2(user, system, idle, iowait, steal, sectors, ticks, ooms int, diskUsedPct int) string {
	it := strconv.Itoa
	cpuLine := func(name string) string {
		// user nice system idle iowait irq softirq steal guest guest_nice
		return name + " " + it(user) + " 0 " + it(system) + " " + it(idle) + " " + it(iowait) + " 0 0 " + it(steal) + " 0 0"
	}
	return strings.Join([]string{
		"@@STAT", cpuLine("cpu"), cpuLine("cpu0"),
		"@@MEM", "MemTotal: 1000 kB", "MemFree: 100 kB", "MemAvailable: 400 kB", "Buffers: 50 kB", "Cached: 300 kB",
		"SReclaimable: 20 kB", "Shmem: 70 kB", "SwapTotal: 0 kB", "SwapFree: 0 kB",
		"@@VMSTAT", "pswpin 10", "pswpout " + it(ooms*2), "oom_kill " + it(ooms),
		"@@DISK", "Filesystem 1-blocks Used Available Capacity Mounted on", "/dev/vda1 1000 " + it(diskUsedPct*10) + " 0 0% /",
		"@@INODES", "Filesystem Inodes IUsed IFree IUse% Mounted on", "/dev/vda1 200 50 150 25% /", "/dev/vdb 0 0 0 - /data",
		"@@DISKSTATS",
		"   8       0 vda " + it(sectors/8) + " 0 " + it(sectors) + " 0 " + it(sectors/16) + " 0 " + it(sectors) + " 0 0 " + it(ticks) + " 0",
		"   8       1 vda1 999 0 99999 0 999 0 99999 0 0 9999 0",
		"   7       0 loop0 999 0 99999 0 999 0 99999 0 0 9999 0",
		"@@DOCKER_PS", "@@DOCKER_STATS", "",
	}, "\n")
}

func TestLevel1Metrics(t *testing.T) {
	_, r1 := parse("id", "srv", sample2(1000, 500, 8000, 100, 0, 8000, 1000, 0, 50), nil)
	r1.at = r1.at.Add(-2 * time.Second)
	// +100 total ticks: user 40, system 10, idle 20, iowait 20, steal 10
	snap, _ := parse("id", "srv", sample2(1040, 510, 8020, 120, 10, 8000+4096, 1000+500, 0, 50), r1)
	h := snap.Host

	if s := h.CPUSplit; s == nil || s.User != 40 || s.System != 10 || s.IOWait != 20 || s.Steal != 10 {
		t.Fatalf("wrong cpu split: %+v", h.CPUSplit)
	}
	if *h.CPU != 60 {
		t.Fatalf("cpu usage must exclude idle and iowait: got %v", *h.CPU)
	}
	if h.Mem.Used != 600*1024 || h.Mem.Cache != (50+300+20-70)*1024 || h.Mem.Available != 400*1024 {
		t.Fatalf("wrong memory breakdown: %+v", h.Mem)
	}
	if len(h.DiskIO) != 1 || h.DiskIO[0].Name != "vda" {
		t.Fatalf("only whole physical disks expected: %+v", h.DiskIO)
	}
	io := h.DiskIO[0]
	// 4096 sectors read and written over ~2s -> ~1 MiB/s each; 500ms busy in ~2s -> ~25% util
	if io.ReadBps < 0.9*(1<<20) || io.ReadBps > 1.1*(1<<20) || io.WriteBps < 0.9*(1<<20) || io.UtilPct < 22 || io.UtilPct > 28 {
		t.Fatalf("wrong disk io: %+v", io)
	}
	if h.IORead == nil || *h.IORead != io.ReadBps {
		t.Fatalf("totals must match devices: %v", h.IORead)
	}
	if len(h.Disks) != 1 || h.Disks[0].InodePct == nil || *h.Disks[0].InodePct != 25 {
		t.Fatalf("wrong inode usage: %+v", h.Disks)
	}
}

func newTestMonitor(t *testing.T) (*Monitor, store.Server) {
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	set := st.Settings()
	set.Alerts.SustainMinutes = 0
	if err := st.SetSettings(set); err != nil {
		t.Fatal(err)
	}
	return &Monitor{st: st, alerts: map[string]map[string]*alertState{}}, store.Server{ID: "s1", Name: "web-1"}
}

func snapWith(cpu, diskPct, ooms float64) *Snapshot {
	return &Snapshot{Status: "online", Host: &Host{
		CPU: &cpu, CPUSplit: &CPUSplit{}, Mem: Mem{Pct: 10}, OOMKills: ooms,
		Disks: []Disk{{Mount: "/", Pct: diskPct}},
	}}
}

func titles(alerts []Alert) string {
	var out []string
	for _, a := range alerts {
		out = append(out, a.Title)
	}
	return strings.Join(out, " | ")
}

func TestThresholdAlerts(t *testing.T) {
	m, srv := newTestMonitor(t)

	s1 := snapWith(50, 50, 0)
	if got := m.thresholdsLocked(srv, nil, s1); len(got) != 0 {
		t.Fatalf("no alert expected: %s", titles(got))
	}
	s2 := snapWith(95, 95, 0)
	got := titles(m.thresholdsLocked(srv, s1, s2))
	if !strings.Contains(got, "High CPU: web-1") || !strings.Contains(got, "Disk / almost full: web-1") {
		t.Fatalf("cpu and disk alerts expected, got %q", got)
	}
	if len(s2.Alerts) != 2 {
		t.Fatalf("2 active alerts expected: %+v", s2.Alerts)
	}
	// Still high -> no repeated notification, still active.
	s3 := snapWith(96, 95, 0)
	if got := m.thresholdsLocked(srv, s2, s3); len(got) != 0 || len(s3.Alerts) != 2 {
		t.Fatalf("no repeat expected: %s / %+v", titles(got), s3.Alerts)
	}
	// Inside the hysteresis band (88% < 90% but > 85%) -> still firing.
	s4 := snapWith(88, 95, 0)
	if got := m.thresholdsLocked(srv, s3, s4); len(got) != 0 || len(s4.Alerts) != 2 {
		t.Fatalf("hysteresis must keep the alert: %s", titles(got))
	}
	// Clearly below -> resolved.
	s5 := snapWith(40, 95, 0)
	if got := titles(m.thresholdsLocked(srv, s4, s5)); got != "Resolved: high CPU on web-1" {
		t.Fatalf("resolve expected, got %q", got)
	}
	// OOM kill counter increased -> event alert.
	s6 := snapWith(40, 95, 2)
	if got := titles(m.thresholdsLocked(srv, s5, s6)); got != "Out of memory: web-1" {
		t.Fatalf("oom alert expected, got %q", got)
	}
}

func TestSustainedAlertWaits(t *testing.T) {
	m, srv := newTestMonitor(t)
	set := m.st.Settings()
	set.Alerts.SustainMinutes = 5
	_ = m.st.SetSettings(set)
	if got := m.thresholdsLocked(srv, nil, snapWith(95, 10, 0)); len(got) != 0 {
		t.Fatalf("CPU alert must wait for the sustain time: %s", titles(got))
	}
	// Pretend the condition started 6 minutes ago.
	m.alerts[srv.ID]["cpu"].since = time.Now().Add(-6 * time.Minute)
	if got := titles(m.thresholdsLocked(srv, nil, snapWith(95, 10, 0))); got != "High CPU: web-1" {
		t.Fatalf("CPU alert expected after sustain time, got %q", got)
	}
}
