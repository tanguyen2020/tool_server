package history

import (
	"encoding/binary"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"serverdash/internal/monitor"
)

func TestContainerIOSeries(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 10, 4, 10, 0, 0, 0, time.Local)
	add := func(at time.Time, rx float64) {
		snap := snapAt(at, 10, 10, nil)
		snap.Docker.Containers = []monitor.Container{{Name: "db", State: "running", CPU: f(1), MemUsed: f(100),
			NetRx: f(rx), NetTx: f(rx / 2), BlkRead: f(4000), BlkWrite: nil}}
		s.Add(snap)
	}
	add(base.Add(10*time.Second), 1000)
	add(base.Add(40*time.Second), 3000)
	add(base.Add(70*time.Second), 500) // flushes minute 0
	time.Sleep(200 * time.Millisecond)
	res, err := s.Query("srv", base, base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if v := res.NetRx["db"]; v == nil || *v[0] != 2000 || *res.NetTx["db"][0] != 1000 || *res.BlkRead["db"][0] != 4000 {
		t.Fatalf("io series wrong: rx=%v tx=%v rd=%v", res.NetRx["db"], res.NetTx["db"], res.BlkRead["db"])
	}
	if _, ok := res.BlkWrite["db"]; ok {
		t.Fatalf("a metric without any data must be omitted")
	}
}

func TestLegacyContainerRecordsStillRead(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 10, 4, 9, 0, 0, 0, time.Local)
	// Write one record in the old "c:" layout: count, then (id, cpu, mem).
	err = s.db.Update(func(tx *bolt.Tx) error {
		id, err := s.nameID(tx, "srv", "old")
		if err != nil {
			return err
		}
		b, _ := tx.CreateBucketIfNotExists([]byte("c:srv"))
		rec := make([]byte, 12)
		binary.LittleEndian.PutUint16(rec, 1)
		binary.LittleEndian.PutUint16(rec[2:], id)
		putF32(rec[4:], 7)
		putF32(rec[8:], 300)
		return b.Put(key(base.UnixMilli()), rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := s.Query("srv", base, base.Add(time.Minute))
	if v := res.CPU["old"]; v == nil || *v[0] != 7 || *res.Mem["old"][0] != 300 {
		t.Fatalf("legacy record not read: %v", res.CPU)
	}
	if _, ok := res.NetRx["old"]; ok {
		t.Fatalf("legacy records have no network data")
	}
}

func TestEvents(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	evs := []monitor.Event{
		{T: now.Add(-2 * time.Hour).UnixMilli(), Container: "api", Action: "die", Detail: "137"},
		{T: now.Add(-2 * time.Hour).UnixMilli(), Container: "api", Action: "start"}, // same ms, different event
		{T: now.Add(-time.Hour).UnixMilli(), Container: "web", Action: "health", Detail: "unhealthy"},
		{T: now.Add(-Retention - time.Hour).UnixMilli(), Container: "old", Action: "stop"},
	}
	if err := s.AddEvents("srv", evs); err != nil {
		t.Fatal(err)
	}
	// The first fetch after a restart covers the last hour again: duplicates must collapse.
	if err := s.AddEvents("srv", evs[:2]); err != nil {
		t.Fatal(err)
	}
	got, err := s.Events("srv", now.Add(-3*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Detail != "137" && got[1].Detail != "137" || got[2].Container != "web" {
		t.Fatalf("expected 3 ordered events, got %+v", got)
	}
	if err := s.Prune(now); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Events("srv", now.Add(-30*24*time.Hour), now)
	if len(all) != 3 {
		t.Fatalf("old events must be pruned, got %+v", all)
	}
	if err := s.DeleteServer("srv"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.Events("srv", now.Add(-3*time.Hour), now); len(left) != 0 {
		t.Fatalf("events must be deleted with the server")
	}
}
