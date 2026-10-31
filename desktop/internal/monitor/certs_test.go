package monitor

import (
	"strings"
	"testing"
	"time"

	"serverdash/internal/hostinfo"
	"serverdash/internal/store"
)

func certIn(subject string, days float64) hostinfo.Cert {
	return hostinfo.Cert{Path: "/etc/ssl/" + subject + ".pem", Subject: subject, NotAfter: time.Now().Add(time.Duration(days*24) * time.Hour).UnixMilli()}
}

func TestCertAlertsRemembered(t *testing.T) {
	dir := t.TempDir()
	srv := store.Server{ID: "s1", Name: "web-1"}
	info := &hostinfo.Maintenance{Certs: []hostinfo.Cert{certIn("shop.example.com", 10.5), certIn("far.example.com", 90)}}

	m := &Monitor{certWarn: loadCertNotes(dir)}
	a := m.certAlertsLocked(srv, info)
	if len(a) != 1 || !strings.Contains(a[0].Title, "shop.example.com") || a[0].Critical {
		t.Fatalf("first check: %+v", a)
	}
	if a := m.certAlertsLocked(srv, info); len(a) != 0 {
		t.Fatalf("the same stage must not be announced twice: %+v", a)
	}
	// A restart: the notes come back from disk.
	m = &Monitor{certWarn: loadCertNotes(dir)}
	if a := m.certAlertsLocked(srv, info); len(a) != 0 {
		t.Fatalf("a restart must not announce it again: %+v", a)
	}
	// Closer to the expiry (same certificate, announced at the 14-day stage): the 3-day stage is announced once.
	closer := &hostinfo.Maintenance{Certs: []hostinfo.Cert{certIn("shop.example.com", 2.1)}}
	m.certWarn.notes["s1|shop.example.com"] = certNote{NotAfter: closer.Certs[0].NotAfter, Stage: 14}
	if a := m.certAlertsLocked(srv, closer); len(a) != 1 || !a[0].Critical {
		t.Fatalf("entering the 3-day stage must be announced: %+v", a)
	}
	// Renewed: far away again, the note is dropped.
	if a := m.certAlertsLocked(srv, &hostinfo.Maintenance{Certs: []hostinfo.Cert{certIn("shop.example.com", 85)}}); len(a) != 0 || len(m.certWarn.notes) != 0 {
		t.Fatalf("a renewed certificate is forgotten: %+v %+v", a, m.certWarn.notes)
	}
}

func TestCertAlertsGroupedAndStale(t *testing.T) {
	srv := store.Server{ID: "s1", Name: "web-1"}
	m := &Monitor{certWarn: loadCertNotes(t.TempDir())}
	info := &hostinfo.Maintenance{Certs: []hostinfo.Cert{
		certIn("a.example.com", 5.5), certIn("b.example.com", 2.5), certIn("c.example.com", -1.5),
		certIn("old.example.com", -200), // left over long ago
	}}
	a := m.certAlertsLocked(srv, info)
	if len(a) != 1 {
		t.Fatalf("one notification per server: %+v", a)
	}
	if !strings.HasPrefix(a[0].Title, "3 certificates need attention on web-1") || !a[0].Critical {
		t.Errorf("title: %q", a[0].Title)
	}
	if strings.Contains(a[0].Body, "old.example.com") || !strings.HasPrefix(a[0].Body, "c.example.com has expired") {
		t.Errorf("body (expired first, stale left out): %q", a[0].Body)
	}
}
