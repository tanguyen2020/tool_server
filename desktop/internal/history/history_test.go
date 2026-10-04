package history

import (
	"testing"
	"time"

	"serverdash/internal/monitor"
)

func f(v float64) *float64 { return &v }

func snapAt(t time.Time, cpu, mem float64, containers map[string][2]float64) *monitor.Snapshot {
	d := &monitor.Docker{}
	for name, v := range containers {
		d.Containers = append(d.Containers, monitor.Container{Name: name, State: "running", CPU: f(v[0]), MemUsed: f(v[1])})
	}
	return &monitor.Snapshot{
		ID: "srv", Status: "online", UpdatedAt: t.UnixMilli(), Docker: d,
		Host: &monitor.Host{CPU: f(cpu), Mem: monitor.Mem{Pct: mem, Used: 1 << 30}, Load: [3]float64{1, 2, 3}},
	}
}

func TestRecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 4, 10, 0, 0, 0, time.Local)
	// Minute 0: two samples averaged; minute 1: one sample; minute 2: nothing (gap); minute 3: one sample.
	s.Add(snapAt(base.Add(5*time.Second), 10, 40, map[string][2]float64{"web": {2, 100}, "db": {8, 500}}))
	s.Add(snapAt(base.Add(35*time.Second), 30, 60, map[string][2]float64{"web": {4, 300}, "db": {8, 500}}))
	s.Add(snapAt(base.Add(65*time.Second), 50, 50, map[string][2]float64{"web": {6, 200}}))
	s.Add(snapAt(base.Add(185*time.Second), 70, 70, map[string][2]float64{"web": {1, 100}}))
	// Offline snapshots are ignored.
	s.Add(&monitor.Snapshot{ID: "srv", Status: "offline", UpdatedAt: base.Add(190 * time.Second).UnixMilli()})
	time.Sleep(200 * time.Millisecond) // background writes
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: data must survive a restart.
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.Query("srv", base, base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Step != 60000 || len(res.Host) != 6 {
		t.Fatalf("expected 1-minute buckets, got step=%d n=%d", res.Step, len(res.Host))
	}
	if res.Host[0].CPU == nil || *res.Host[0].CPU != 20 || *res.Host[0].Mem != 50 {
		t.Fatalf("minute 0 must average two samples: %+v", res.Host[0])
	}
	if res.Host[1].CPU == nil || *res.Host[1].CPU != 50 {
		t.Fatalf("minute 1 wrong: %+v", res.Host[1])
	}
	if res.Host[2].CPU != nil || res.Host[2].Load1 != nil {
		t.Fatalf("minute 2 must be a gap: %+v", res.Host[2])
	}
	if res.Host[3].CPU == nil || *res.Host[3].CPU != 70 || *res.Host[3].Load15 != 3 {
		t.Fatalf("minute 3 (written on Close) wrong: %+v", res.Host[3])
	}
	if res.Host[0].IORead != nil {
		t.Fatalf("missing values must stay nil: %+v", res.Host[0].IORead)
	}
	if web := res.CPU["web"]; web == nil || *web[0] != 3 || *res.Mem["web"][0] != 200 || web[2] != nil {
		t.Fatalf("container web wrong: %v", res.CPU["web"])
	}
	if db := res.CPU["db"]; db == nil || *db[0] != 8 || db[1] != nil {
		t.Fatalf("container db wrong: %v", res.CPU["db"])
	}
}

func TestLongRangeIsDownsampled(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 3*24*60; i++ { // 3 days, one sample per minute
		s.Add(snapAt(base.Add(time.Duration(i)*time.Minute), float64(i%100), 50, nil))
	}
	time.Sleep(500 * time.Millisecond)
	res, err := s.Query("srv", base, base.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Host) > MaxPoints+1 || res.Step != 6*60000 {
		t.Fatalf("expected ~720 buckets of 6 minutes, got %d buckets step %d", len(res.Host), res.Step)
	}
	if res.Host[10].CPU == nil {
		t.Fatalf("bucket must hold averaged data")
	}
}

func TestPruneAndDelete(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	old := now.Add(-Retention - time.Hour)
	s.Add(snapAt(old, 10, 10, map[string][2]float64{"web": {1, 1}}))
	s.Add(snapAt(old.Add(time.Minute), 10, 10, map[string][2]float64{"web": {1, 1}})) // flushes the old minute
	s.Add(snapAt(now.Add(-time.Hour), 20, 20, map[string][2]float64{"web": {1, 1}}))
	s.Add(snapAt(now.Add(-time.Hour+time.Minute), 20, 20, nil))
	time.Sleep(300 * time.Millisecond)
	if err := s.Prune(now); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Query("srv", old.Add(-time.Minute), now)
	count := 0
	for _, p := range res.Host {
		if p.CPU != nil {
			count++
			if *p.CPU != 20 {
				t.Fatalf("old data must be pruned, found cpu=%v", *p.CPU)
			}
		}
	}
	if count == 0 {
		t.Fatalf("recent data must remain")
	}
	if err := s.DeleteServer("srv"); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Query("srv", now.Add(-2*time.Hour), now)
	for _, p := range res.Host {
		if p.CPU != nil {
			t.Fatalf("data must be gone after DeleteServer")
		}
	}
}
