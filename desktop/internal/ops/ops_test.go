package ops

import (
	"strings"
	"testing"

	"serverdash/internal/store"
)

func TestParseServices(t *testing.T) {
	out := `cron.service loaded active running Regular background program processing daemon
nginx.service loaded failed failed A high performance web server
ssh.service loaded active running OpenBSD Secure Shell server
@@FILES
cron.service enabled enabled
nginx.service enabled enabled
getty@.service enabled enabled
rsync.service disabled enabled
ssh.service enabled enabled
`
	list := parseServices(out)
	if len(list) != 4 {
		t.Fatalf("want 4 services (template skipped), got %d: %+v", len(list), list)
	}
	byName := map[string]Service{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if n := byName["nginx.service"]; n.Active != "failed" || n.Enabled != "enabled" || n.Description != "A high performance web server" {
		t.Errorf("nginx: %+v", n)
	}
	if r := byName["rsync.service"]; r.Active != "inactive" || r.Enabled != "disabled" {
		t.Errorf("rsync (unit file only): %+v", r)
	}
	if list[0].Name != "cron.service" {
		t.Errorf("not sorted: %s first", list[0].Name)
	}
}

func TestValidService(t *testing.T) {
	for _, ok := range []string{"nginx.service", "getty@tty1.service", "systemd-journald.service", `dev-disk-by\x2duuid.service`} {
		if ValidService(ok) != nil {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"nginx", "a;rm -rf /.service", "$(x).service", "-x.service", "a b.service"} {
		if ValidService(bad) == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestParsePorts(t *testing.T) {
	out := `@@ROOT:0
tcp   LISTEN 0      4096         0.0.0.0:80        0.0.0.0:*    users:(("docker-proxy",pid=1201,fd=4))
tcp   LISTEN 0      4096            [::]:80           [::]:*    users:(("docker-proxy",pid=1208,fd=4))
tcp   LISTEN 0      128          0.0.0.0:22        0.0.0.0:*    users:(("sshd",pid=812,fd=3))
tcp   ESTAB  0      0          10.0.0.5:22       10.0.0.9:5122  users:(("sshd",pid=900,fd=4))
udp   UNCONN 0      0      127.0.0.53%lo:53        0.0.0.0:*    users:(("systemd-resolve",pid=500,fd=13))
tcp   LISTEN 0      511        127.0.0.1:6379      0.0.0.0:*
`
	ports := parsePorts(out)
	if len(ports) != 4 {
		t.Fatalf("want 4 ports, got %d: %+v", len(ports), ports)
	}
	if p := ports[0]; p.Port != 22 || p.Process != "sshd" || p.PID != 812 {
		t.Errorf("first: %+v", p)
	}
	if p := ports[1]; p.Proto != "udp" || p.Port != 53 || p.Addrs[0] != "127.0.0.53" {
		t.Errorf("dns: %+v", p)
	}
	// docker-proxy on 0.0.0.0 and [::] with different PIDs are two rows; the same process would merge.
	if p := ports[2]; p.Port != 80 || p.Process != "docker-proxy" {
		t.Errorf("http: %+v", p)
	}
	if p := ports[3]; p.Port != 6379 || p.Process != "" {
		t.Errorf("redis (no process info): %+v", p)
	}
}

func TestComposeScript(t *testing.T) {
	p := ComposeProject{Name: "shop", Dir: "/srv/shop it's", Files: []string{"/srv/shop it's/docker-compose.yml", "/srv/shop it's/override.yml"}}
	script, title, err := ComposeScript(store.Server{UseSudo: true}, p, "update")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`cd '/srv/shop it'\''s'`,
		`if sudo -n docker compose version`,
		`DC="sudo -n docker-compose"`,
		`dc() { $DC -p shop -f '/srv/shop it'\''s/docker-compose.yml' -f '/srv/shop it'\''s/override.yml' "$@"; }`,
		"dc pull && dc up -d --remove-orphans",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if title != "shop: pull images and recreate" {
		t.Errorf("title %q", title)
	}
	if _, _, err := ComposeScript(store.Server{}, p, "rm -rf"); err == nil {
		t.Error("unknown action accepted")
	}
}

func TestQuote(t *testing.T) {
	if got := Quote(`a'b c`); got != `'a'\''b c'` {
		t.Errorf("Quote: %s", got)
	}
}
