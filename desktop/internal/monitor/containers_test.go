package monitor

import (
	"strings"
	"testing"
	"time"

	"serverdash/internal/store"
)

func TestHealthAndExitCode(t *testing.T) {
	d := parseDocker([]string{
		`{"ID":"a","Names":"web","State":"running","Status":"Up 3 hours (healthy)"}`,
		`{"ID":"b","Names":"api","State":"running","Status":"Up 2 minutes (unhealthy)"}`,
		`{"ID":"c","Names":"job","State":"running","Status":"Up 5 seconds (health: starting)"}`,
		`{"ID":"d","Names":"old","State":"exited","Status":"Exited (137) 2 days ago"}`,
		`{"ID":"e","Names":"plain","State":"running","Status":"Up 1 hour"}`,
	}, nil)
	got := map[string]Container{}
	for _, c := range d.Containers {
		got[c.Name] = c
	}
	if got["web"].Health != "healthy" || got["api"].Health != "unhealthy" || got["job"].Health != "starting" || got["plain"].Health != "" {
		t.Fatalf("wrong health: %+v", got)
	}
	if got["old"].ExitCode == nil || *got["old"].ExitCode != 137 || got["web"].ExitCode != nil {
		t.Fatalf("wrong exit codes: %+v / %+v", got["old"].ExitCode, got["web"].ExitCode)
	}
	if d.Unhealthy != 1 {
		t.Fatalf("1 unhealthy expected, got %d", d.Unhealthy)
	}
	if r := parseInspect([]string{"a 0 0 bridge", "b 12 4242 host", "junk"}); r["b"].restarts != 12 || r["b"].pid != 4242 || r["b"].netMode != "host" || len(r) != 2 {
		t.Fatalf("wrong inspect: %v", r)
	}
}

func dockerSnap(restarting bool, health string) *Snapshot {
	return &Snapshot{Status: "online", Docker: &Docker{Containers: []Container{
		{ID: "id-api", Name: "api", State: "running", Health: health},
		{ID: "id-web", Name: "web", State: "running"},
	}}}
}

func TestRestartLoopAndAlerts(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	m := &Monitor{st: st, expected: map[string]time.Time{}, fails: map[string]int{},
		restarts: map[string]map[string]int{}, restartsAt: map[string]time.Time{}, restartLog: map[string]map[string][]time.Time{}}
	srv := store.Server{ID: "s", Name: "web-1"}

	s1 := dockerSnap(false, "healthy")
	m.applyRestartsLocked("s", s1, map[string]int{"id-api": 5, "id-web": 0})
	if *s1.Docker.Containers[0].RestartCount != 5 || s1.Docker.Containers[0].Looping {
		t.Fatalf("first sample: count 5, not looping: %+v", s1.Docker.Containers[0])
	}
	// Cached counts are reused when a cycle does not fetch them.
	s2 := dockerSnap(false, "unhealthy")
	m.applyRestartsLocked("s", s2, nil)
	if *s2.Docker.Containers[0].RestartCount != 5 {
		t.Fatalf("cached count expected")
	}
	if got := m.detectLocked(srv, s1, s2); len(got) != 1 || !strings.HasPrefix(got[0].Title, "Container unhealthy: api") {
		t.Fatalf("unhealthy alert expected, got %+v", got)
	}
	// 3 more restarts since the last fetch -> restart loop.
	s3 := dockerSnap(true, "healthy")
	m.applyRestartsLocked("s", s3, map[string]int{"id-api": 8, "id-web": 0})
	if !s3.Docker.Containers[0].Looping || s3.Docker.Containers[1].Looping {
		t.Fatalf("api must be looping, web not: %+v", s3.Docker.Containers)
	}
	alerts := m.detectLocked(srv, s2, s3)
	var titles []string
	for _, a := range alerts {
		titles = append(titles, a.Title)
	}
	joined := strings.Join(titles, " | ")
	if !strings.Contains(joined, "Container healthy again: api") || !strings.Contains(joined, "Container restarting repeatedly: api") {
		t.Fatalf("recovery and loop alerts expected, got %q", joined)
	}
	// The loop clears once the restarts are older than the window.
	for i := range m.restartLog["s"]["api"] {
		m.restartLog["s"]["api"][i] = time.Now().Add(-loopWindow - time.Minute)
	}
	s4 := dockerSnap(false, "healthy")
	m.applyRestartsLocked("s", s4, map[string]int{"id-api": 8, "id-web": 0})
	if s4.Docker.Containers[0].Looping {
		t.Fatalf("loop must clear after the window")
	}
}
