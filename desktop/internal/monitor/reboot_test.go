package monitor

import (
	"strings"
	"testing"
	"time"

	"serverdash/internal/store"
)

func TestRebootTracking(t *testing.T) {
	srv := store.Server{ID: "s1", Name: "web-1"}
	m := &Monitor{
		snaps: map[string]*Snapshot{}, fails: map[string]int{}, maintAt: map[string]time.Time{},
		reboots: map[string]*rebootState{},
	}
	m.snaps["s1"] = &Snapshot{Status: "online", Docker: &Docker{Containers: []Container{
		{Name: "api", State: "running"}, {Name: "worker", State: "running"}, {Name: "old", State: "exited"},
	}}}
	m.ExpectReboot("s1")
	m.reboots["s1"].at = time.Now().Add(-90 * time.Second)

	// Still up (shutting down): old uptime -> rebooting, no alerts.
	snap := &Snapshot{Status: "online", Host: &Host{Uptime: 86400}}
	if busy, alerts := m.rebootLocked(srv, snap, true); !busy || alerts != nil || snap.Status != "rebooting" || snap.RebootingSince == 0 {
		t.Fatalf("shutting down: busy=%v alerts=%v status=%s", busy, alerts, snap.Status)
	}
	// Down.
	snap = &Snapshot{Status: "offline"}
	if busy, _ := m.rebootLocked(srv, snap, false); !busy || snap.Status != "rebooting" {
		t.Fatalf("down: busy=%v status=%s", busy, snap.Status)
	}
	// Back with a fresh uptime; "worker" did not start again.
	m.fails["s1"] = 5
	snap = &Snapshot{Status: "online", Host: &Host{Uptime: 40}, Docker: &Docker{Containers: []Container{
		{Name: "api", State: "running"}, {Name: "worker", State: "exited"},
	}}}
	busy, alerts := m.rebootLocked(srv, snap, true)
	if busy || len(alerts) != 1 || !alerts[0].Critical || !strings.Contains(alerts[0].Body, "worker") || strings.Contains(alerts[0].Body, "old") {
		t.Fatalf("back: busy=%v alerts=%+v", busy, alerts)
	}
	if snap.Status != "online" || m.fails["s1"] != 0 || m.reboots["s1"] != nil {
		t.Fatalf("state not reset: status=%s fails=%d", snap.Status, m.fails["s1"])
	}

	// Never comes back.
	m.ExpectReboot("s1")
	m.reboots["s1"].at = time.Now().Add(-rebootTimeout - time.Minute)
	snap = &Snapshot{Status: "offline"}
	busy, alerts = m.rebootLocked(srv, snap, false)
	if busy || len(alerts) != 1 || !strings.HasPrefix(alerts[0].Title, "Server not back") || snap.Status != "offline" {
		t.Fatalf("timeout: busy=%v alerts=%+v status=%s", busy, alerts, snap.Status)
	}
}
