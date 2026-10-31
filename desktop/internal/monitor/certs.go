package monitor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"serverdash/internal/hostinfo"
	"serverdash/internal/store"
)

// Certificate expiry notifications are remembered on disk, so starting the app does not announce them again.
// A certificate is announced once per stage (14, 7, 3, 1 days left, then expired); a renewed certificate
// (a new expiry date) starts over. Each server sends at most one notification per check.

// CertWarnDays: certificates expiring within this many days raise an alert.
const CertWarnDays = 14

// staleExpired: a certificate already expired this long when first seen is left over, not news.
const staleExpired = 7

type certNote struct {
	NotAfter int64 `json:"notAfter"`
	Stage    int   `json:"stage"`
}

type certNotes struct {
	path  string
	notes map[string]certNote // server id | domain (or path) -> last announced stage
}

func loadCertNotes(dir string) *certNotes {
	n := &certNotes{path: filepath.Join(dir, "certalerts.json"), notes: map[string]certNote{}}
	if dir == "" {
		n.path = ""
		return n
	}
	if b, err := os.ReadFile(n.path); err == nil {
		_ = json.Unmarshal(b, &n.notes)
	}
	return n
}

func (n *certNotes) save() {
	if n.path == "" {
		return
	}
	b, err := json.MarshalIndent(n.notes, "", " ")
	if err != nil {
		return
	}
	tmp := n.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, n.path)
	}
}

// certStage is the alert stage of a certificate: 14, 7, 3, 1 days left, 0 once expired, -1 when far away.
func certStage(days int) int {
	switch {
	case days < 0:
		return 0
	case days <= 1:
		return 1
	case days <= 3:
		return 3
	case days <= 7:
		return 7
	case days <= CertWarnDays:
		return 14
	}
	return -1
}

func certKey(serverID string, c hostinfo.Cert) string {
	name := strings.ToLower(c.Subject)
	if name == "" {
		name = "path:" + c.Path
	}
	return serverID + "|" + name
}

// certAlertsLocked returns the notification for the certificates of one server that entered a new stage.
func (m *Monitor) certAlertsLocked(srv store.Server, info *hostinfo.Maintenance) []Alert {
	now := time.Now()
	type due struct {
		name string
		days int
	}
	var news []due
	changed := false
	seen := map[string]bool{}
	for _, c := range info.Certs {
		key := certKey(srv.ID, c)
		seen[key] = true
		days := c.DaysLeft(now)
		stage := certStage(days)
		old, known := m.certWarn.notes[key]
		if stage < 0 {
			if known {
				delete(m.certWarn.notes, key)
				changed = true
			}
			continue
		}
		if known && old.NotAfter == c.NotAfter && stage >= old.Stage {
			continue // already announced at this stage
		}
		m.certWarn.notes[key] = certNote{NotAfter: c.NotAfter, Stage: stage}
		changed = true
		if !known && days < -staleExpired {
			continue // long expired when first seen: shown on the server page, not pushed
		}
		name := c.Subject
		if name == "" {
			name = c.Path
		}
		news = append(news, due{name, days})
	}
	// Certificates no longer on the server (renewed under another name, removed) are forgotten.
	for key := range m.certWarn.notes {
		if strings.HasPrefix(key, srv.ID+"|") && !seen[key] {
			delete(m.certWarn.notes, key)
			changed = true
		}
	}
	if changed {
		m.certWarn.save()
	}
	if len(news) == 0 {
		return nil
	}
	sort.Slice(news, func(i, j int) bool { return news[i].days < news[j].days })
	when := func(d due) string {
		if d.days < 0 {
			return "has expired"
		}
		return fmt.Sprintf("expires in %d day%s", d.days, map[bool]string{true: "", false: "s"}[d.days == 1])
	}
	critical := news[0].days <= 3
	if len(news) == 1 {
		return []Alert{{"Certificate " + when(news[0]) + ": " + news[0].name, srv.Name, critical}}
	}
	lines := make([]string, 0, len(news))
	for i, d := range news {
		if i == 5 {
			lines = append(lines, fmt.Sprintf("and %d more", len(news)-5))
			break
		}
		lines = append(lines, d.name+" "+when(d))
	}
	return []Alert{{fmt.Sprintf("%d certificates need attention on %s", len(news), srv.Name), strings.Join(lines, "\n"), critical}}
}
