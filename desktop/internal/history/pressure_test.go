package history

import (
	"math"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"serverdash/internal/monitor"
)

func TestNewValuesDisksAndForecast(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 4, 0, 0, 0, 0, time.Local)
	const gib = float64(1 << 30)

	// A record written by an older version: 15 host values only.
	old := make([]byte, nHostV1*4)
	for j := 0; j < nHostV1; j++ {
		putF32(old[j*4:], float64(j+1))
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("h:srv"))
		if err != nil {
			return err
		}
		return b.Put(key(base.Add(-time.Minute).UnixMilli()), old)
	}); err != nil {
		t.Fatal(err)
	}

	// One sample an hour for 12 hours: the root disk grows 1 GiB an hour.
	for h := 0; h < 12; h++ {
		snap := snapAt(base.Add(time.Duration(h)*time.Hour), 10, 50, map[string][2]float64{"api": {5, 100}})
		snap.Host.PSI = &monitor.PSI{CPU: f(3), IO: f(12)}
		snap.Host.TCP = &monitor.TCP{InUse: 40, TimeWait: 9}
		snap.Host.IOAwait = f(2.5)
		snap.Host.Swap = monitor.Mem{Used: 64 << 20}
		snap.Host.Disks = []monitor.Disk{
			{Mount: "/", Size: 100 * gib, Used: (10 + float64(h)) * gib, Pct: 10 + float64(h)},
			{Mount: "/data", Size: 500 * gib, Used: 200 * gib, Pct: 40},
		}
		snap.Docker.Containers[0].MemLimitPct = f(25)
		snap.Docker.Containers[0].Throttled = f(7)
		s.Add(snap)
	}
	time.Sleep(200 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := s.Query("srv", base.Add(-time.Minute), base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if p := res.Host[0]; p.IOWrite == nil || *p.IOWrite != 15 || p.PSICPU != nil || p.TCPInUse != nil {
		t.Fatalf("old record must keep its 15 values and leave the new ones empty: %+v", p)
	}
	if p := res.Host[1]; p.PSICPU == nil || *p.PSICPU != 3 || *p.PSIIO != 12 || *p.TCPInUse != 40 ||
		*p.TCPTimeWait != 9 || *p.IOAwait != 2.5 || *p.SwapUsed != 64<<20 || p.PSIMem != nil {
		t.Fatalf("new host values wrong: %+v", p)
	}
	if v := res.MemLimit["api"]; v == nil || *v[1] != 25 || *res.Throttled["api"][1] != 7 {
		t.Fatalf("container limit / throttling wrong: %v %v", res.MemLimit, res.Throttled)
	}
	if v := res.Disks["/"]; v == nil || *v[1] != 10 || *res.Disks["/data"][1] != 40 {
		t.Fatalf("disk history wrong: %v", res.Disks)
	}
	if _, ok := res.CPU["disk:/"]; ok {
		t.Fatal("disk names must not show up as containers")
	}

	fc, err := s.DiskForecast("srv", base.Add(12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	byMount := map[string]Forecast{}
	for _, x := range fc {
		byMount[x.Mount] = x
	}
	root := byMount["/"]
	if math.Abs(root.PerDay-24*gib) > gib/100 || root.DaysLeft == nil || math.Abs(*root.DaysLeft-79.0/24) > 0.01 {
		t.Fatalf("root forecast wrong: %+v days=%v", root, root.DaysLeft)
	}
	if d := byMount["/data"]; d.Mount == "" || d.DaysLeft != nil {
		t.Fatalf("a flat disk has no days left: %+v", d)
	}
	if fc, _ := s.DiskForecast("other", base); len(fc) != 0 {
		t.Fatalf("no data must give no forecast: %v", fc)
	}
}
