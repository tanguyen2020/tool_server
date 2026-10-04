package hostinfo

import (
	"strings"
	"testing"
	"time"
)

func TestParseTop(t *testing.T) {
	out := strings.Join([]string{
		"    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND",
		"   1234 postgres  20   0  220.1m 123456  10000 R  45.5   1.6   1:23.45 postgres: checkpointer",
		"    987 root      20   0    2.1g   1.2g  50000 S  12.0  15.2  10:00.01 /usr/bin/dockerd -H fd://",
		"     42 www-data  20   0   10000   5000   2000 S   0.0   0.1   0:00.10 nginx: worker process",
		"@@CGROUP",
		"1234 " + strings.Repeat("a", 64),
		"987 ",
		"42 " + strings.Repeat("b", 64),
	}, "\n")
	p := parseTop(out)
	if len(p) != 3 {
		t.Fatalf("3 processes expected, got %d", len(p))
	}
	if p[0].PID != 1234 || p[0].CPU != 45.5 || p[0].RSS != 123456*1024 || p[0].Command != "postgres: checkpointer" || p[0].ContainerID != strings.Repeat("a", 64) {
		t.Fatalf("wrong first process: %+v", p[0])
	}
	if p[1].RSS != 1.2*(1<<30) || p[1].ContainerID != "" || p[1].Command != "/usr/bin/dockerd -H fd://" {
		t.Fatalf("wrong dockerd: %+v", p[1])
	}
}

func TestParseMaintenance(t *testing.T) {
	out := strings.Join([]string{
		"@@REBOOTFILE",
		"@@REBOOTPKGS",
		"@@RUNNING", "6.1.0-25-amd64",
		"@@NEWEST", "6.1.0-41-amd64",
		"@@UPGRADES",
		"Inst openssl [3.0.11-1~deb12u2] (3.0.13-1~deb12u1 Debian-Security:12/stable-security [amd64])",
		"Inst libssl3 [3.0.11-1~deb12u2] (3.0.13-1~deb12u1 Debian-Security:12/stable-security [amd64]) []",
		"Inst tzdata [2024a-0+deb12u1] (2025b-0+deb12u1 Debian:12.11/stable [all])",
		"Inst newpkg (1.0-1 Debian:12.11/stable [amd64])",
		"@@LISTS", "1759550000",
	}, "\n")
	m := parseMaintenance(out, time.Now())
	if !m.RebootRequired || !strings.Contains(m.RebootReason, "6.1.0-41-amd64 is installed") {
		t.Fatalf("kernel reboot expected: %+v", m)
	}
	if m.Updates != 4 || m.SecurityUpdates != 2 {
		t.Fatalf("wrong counts: %+v", m)
	}
	if m.Packages[0] != "libssl3 3.0.13-1~deb12u1 (security)" || m.Packages[3] != "tzdata 2025b-0+deb12u1" {
		t.Fatalf("security packages must come first: %v", m.Packages)
	}
	if m.ListsUpdated != 1759550000000 {
		t.Fatalf("wrong lists time: %d", m.ListsUpdated)
	}

	clean := parseMaintenance("@@REBOOTFILE\n@@RUNNING\n6.1.0-41-amd64\n@@NEWEST\n6.1.0-41-amd64\n@@UPGRADES\n@@LISTS\n", time.Now())
	if clean.RebootRequired || clean.Updates != 0 || clean.ListsUpdated != 0 {
		t.Fatalf("clean host expected: %+v", clean)
	}
	file := parseMaintenance("@@REBOOTFILE\nyes\n@@REBOOTPKGS\nlibc6\n@@RUNNING\nx\n@@NEWEST\nx\n", time.Now())
	if !file.RebootRequired || file.RebootReason != "required by updated packages: libc6" {
		t.Fatalf("reboot file expected: %+v", file)
	}
}

func TestParseCerts(t *testing.T) {
	certs := parseCerts([]string{
		"/etc/letsencrypt/live/example.com/cert.pem|Mar  1 12:00:00 2027 GMT|example.com",
		"/etc/nginx/ssl/old.crt|Jan 15 08:30:00 2026 GMT|old.example.com",
		"/etc/nginx/ssl/broken.crt|not a date|x",
		"/etc/letsencrypt/live/example.com/cert.pem|Mar  1 12:00:00 2027 GMT|example.com",
	})
	if len(certs) != 2 {
		t.Fatalf("want 2 certs, got %+v", certs)
	}
	if certs[0].Subject != "old.example.com" || certs[1].Subject != "example.com" {
		t.Errorf("not sorted by expiry: %+v", certs)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d := certs[0].DaysLeft(now); d != 14 {
		t.Errorf("days left %d, want 14", d)
	}
}
