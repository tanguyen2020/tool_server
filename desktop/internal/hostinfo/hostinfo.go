// Package hostinfo runs the on-demand and slow-changing host checks:
// top processes (on demand) and maintenance status (reboot needed, pending updates; hourly).
package hostinfo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"serverdash/internal/sshpool"
)

// ------------------------------------------------------------------ top processes

type Process struct {
	PID         int     `json:"pid"`
	User        string  `json:"user"`
	State       string  `json:"state"`
	CPU         float64 `json:"cpu"` // % of one core over the last second
	Mem         float64 `json:"mem"` // % of RAM
	RSS         float64 `json:"rss"` // bytes
	Time        string  `json:"time"`
	Command     string  `json:"command"`
	ContainerID string  `json:"containerId,omitempty"`
}

var sortFields = map[string]string{"cpu": "%CPU", "mem": "%MEM"}

// The second `top` iteration measures CPU over the last second (the first one is since boot).
// The cgroup of each process tells which Docker container it belongs to.
func topScript(field string) string {
	return `export LC_ALL=C
out=$(COLUMNS=512 top -b -c -w 512 -n 2 -d 1 -o ` + field + ` 2>/dev/null | awk '/^top -/{n++} n==2' | sed -n '/^ *PID /,$p' | head -n 26)
[ -z "$out" ] && { echo "top is not available (install the procps package)" >&2; exit 1; }
echo "$out"
echo @@CGROUP
echo "$out" | awk 'NR>1{print $1}' | while read -r p; do echo "$p $(grep -m1 -oE '[0-9a-f]{64}' /proc/$p/cgroup 2>/dev/null)"; done
`
}

var memSuffix = map[byte]float64{'k': 1 << 10, 'm': 1 << 20, 'g': 1 << 30, 't': 1 << 40, 'p': 1 << 50}

// parseKiB reads top's memory columns: plain numbers are KiB, larger values carry a unit suffix.
func parseKiB(s string) float64 {
	if s == "" {
		return 0
	}
	if mult, ok := memSuffix[s[len(s)-1]]; ok {
		v, _ := strconv.ParseFloat(s[:len(s)-1], 64)
		return v * mult
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v * 1024
}

func parseTop(out string) []Process {
	head, cg, _ := strings.Cut(out, "@@CGROUP")
	containers := map[int]string{}
	for _, l := range strings.Split(cg, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			pid, _ := strconv.Atoi(f[0])
			containers[pid] = f[1]
		}
	}
	var procs []Process
	for i, l := range strings.Split(strings.TrimSpace(head), "\n") {
		f := strings.Fields(l)
		// PID USER PR NI VIRT RES SHR S %CPU %MEM TIME+ COMMAND...
		if i == 0 || len(f) < 12 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(f[8], 64)
		mem, _ := strconv.ParseFloat(f[9], 64)
		procs = append(procs, Process{
			PID: pid, User: f[1], RSS: parseKiB(f[5]), State: f[7], CPU: cpu, Mem: mem, Time: f[10],
			Command: strings.Join(f[11:], " "), ContainerID: containers[pid],
		})
	}
	return procs
}

func TopProcesses(ctx context.Context, conn *sshpool.Conn, sortBy string) ([]Process, error) {
	field, ok := sortFields[sortBy]
	if !ok {
		return nil, errors.New("invalid sort field")
	}
	res, err := conn.Exec(ctx, topScript(field), 20*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("cannot list processes: %s", strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return parseTop(res.Stdout), nil
}

// ------------------------------------------------------------------ maintenance

type Maintenance struct {
	RebootRequired  bool     `json:"rebootRequired"`
	RebootReason    string   `json:"rebootReason,omitempty"`
	Updates         int      `json:"updates"`
	SecurityUpdates int      `json:"securityUpdates"`
	Packages        []string `json:"packages"`     // "name new-version", security first, at most 50
	ListsUpdated    int64    `json:"listsUpdated"` // unix ms of the last `apt update`, 0 if unknown
	Certs           []Cert   `json:"certs"`        // TLS certificates found on the server, soonest expiry first
	CheckedAt       int64    `json:"checkedAt"`
}

// Cert is a TLS certificate file used by Let's Encrypt, nginx or Apache on the server.
type Cert struct {
	Path     string `json:"path"`
	Subject  string `json:"subject"`
	NotAfter int64  `json:"notAfter"` // unix ms
}

// DaysLeft until the certificate expires (negative once expired).
func (c Cert) DaysLeft(now time.Time) int {
	return int(time.UnixMilli(c.NotAfter).Sub(now).Hours() / 24)
}

// apt-get -s only simulates (no root needed, no changes) and reads the local package lists.
const maintenanceScript = `export LC_ALL=C
echo @@REBOOTFILE; [ -f /var/run/reboot-required ] && echo yes
echo @@REBOOTPKGS; sort -u /var/run/reboot-required.pkgs 2>/dev/null | head -n 10
echo @@RUNNING; uname -r
echo @@NEWEST; ls -1 /boot/vmlinuz-* 2>/dev/null | sed 's|^/boot/vmlinuz-||' | sort -V | tail -n 1
echo @@UPGRADES; apt-get -s -o Debug::NoLocking=1 dist-upgrade 2>/dev/null | grep '^Inst '
echo @@LISTS; stat -c %Y /var/lib/apt/lists/*Release 2>/dev/null | sort -n | tail -n 1
echo @@CERTS
S=""; if [ "$(id -u)" -ne 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi
if command -v openssl >/dev/null 2>&1; then
  { $S sh -c 'ls -1 /etc/letsencrypt/live/*/cert.pem' 2>/dev/null
    grep -rhoE '^[[:space:]]*ssl_certificate[[:space:]]+[^;]+' /etc/nginx 2>/dev/null | awk '{print $2}'
    grep -rhoiE '^[[:space:]]*SSLCertificateFile[[:space:]]+[^[:space:]]+' /etc/apache2 2>/dev/null | awk '{print $2}'
  } | tr -d '"' | grep -v '\$' | sort -u | head -n 40 | while read -r f; do
    out=$($S openssl x509 -noout -enddate -subject -in "$f" 2>/dev/null) || continue
    end=$(printf '%s\n' "$out" | sed -n 's/^notAfter=//p')
    cn=$(printf '%s\n' "$out" | sed -n 's/^subject=.*CN *= *\([^,/]*\).*/\1/p')
    echo "$f|$end|$cn"
  done
fi
`

// Inst openssl [3.0.11-1~deb12u2] (3.0.13-1~deb12u1 Debian-Security:12/stable-security [amd64])
var instRe = regexp.MustCompile(`^Inst (\S+) (?:\[[^\]]*\] )?\((\S+) ([^\[]*)`)

func parseMaintenance(out string, now time.Time) *Maintenance {
	sec := map[string][]string{}
	cur := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "@@") {
			cur = strings.TrimSpace(l[2:])
			continue
		}
		if strings.TrimSpace(l) != "" {
			sec[cur] = append(sec[cur], strings.TrimSpace(l))
		}
	}
	first := func(k string) string {
		if len(sec[k]) == 0 {
			return ""
		}
		return sec[k][0]
	}
	m := &Maintenance{CheckedAt: now.UnixMilli(), Packages: []string{}}
	running, newest := first("RUNNING"), first("NEWEST")
	switch {
	case newest != "" && running != "" && newest != running:
		m.RebootRequired = true
		m.RebootReason = fmt.Sprintf("kernel %s is installed, %s is running", newest, running)
	case first("REBOOTFILE") == "yes":
		m.RebootRequired = true
		m.RebootReason = "required by updated packages"
		if pkgs := sec["REBOOTPKGS"]; len(pkgs) > 0 {
			m.RebootReason += ": " + strings.Join(pkgs, ", ")
		}
	}
	var secPkgs, otherPkgs []string
	for _, l := range sec["UPGRADES"] {
		mm := instRe.FindStringSubmatch(l)
		if mm == nil {
			continue
		}
		m.Updates++
		entry := mm[1] + " " + mm[2]
		if strings.Contains(strings.ToLower(mm[3]), "security") {
			m.SecurityUpdates++
			secPkgs = append(secPkgs, entry+" (security)")
		} else {
			otherPkgs = append(otherPkgs, entry)
		}
	}
	sort.Strings(secPkgs)
	sort.Strings(otherPkgs)
	m.Packages = append(secPkgs, otherPkgs...)
	if len(m.Packages) > 50 {
		m.Packages = m.Packages[:50]
	}
	if ts, err := strconv.ParseInt(first("LISTS"), 10, 64); err == nil {
		m.ListsUpdated = ts * 1000
	}
	m.Certs = parseCerts(sec["CERTS"])
	return m
}

// parseCerts reads "path|notAfter|CN" lines; notAfter looks like "Mar  1 12:00:00 2026 GMT".
func parseCerts(lines []string) []Cert {
	certs := []Cert{}
	seen := map[string]bool{}
	for _, l := range lines {
		parts := strings.SplitN(l, "|", 3)
		if len(parts) < 3 || seen[parts[0]] {
			continue
		}
		t, err := time.Parse("Jan 2 15:04:05 2006 MST", strings.Join(strings.Fields(parts[1]), " "))
		if err != nil {
			continue
		}
		seen[parts[0]] = true
		certs = append(certs, Cert{Path: parts[0], Subject: parts[2], NotAfter: t.UnixMilli()})
	}
	sort.Slice(certs, func(i, j int) bool { return certs[i].NotAfter < certs[j].NotAfter })
	return certs
}

func CheckMaintenance(ctx context.Context, conn *sshpool.Conn) (*Maintenance, error) {
	res, err := conn.Exec(ctx, maintenanceScript, 60*time.Second)
	if err != nil {
		return nil, err
	}
	return parseMaintenance(res.Stdout, time.Now()), nil
}

// ------------------------------------------------------------------ reboot

// rebootScript reboots with root rights: directly as root, otherwise through passwordless sudo.
// `systemctl reboot` only queues the job and returns; /sbin/reboot is the fallback without systemd.
const rebootScript = `if [ "$(id -u)" -eq 0 ]; then systemctl reboot || /sbin/reboot; else sudo -n systemctl reboot || sudo -n /sbin/reboot; fi`

// Reboot asks the server to restart. The SSH connection usually drops before the command reports back:
// that counts as success, the monitor then follows the server until it is up again.
func Reboot(ctx context.Context, conn *sshpool.Conn) error {
	// Make sure the server is reachable first: after this, a broken connection means it is going down.
	if _, err := conn.Exec(ctx, "true", 15*time.Second); err != nil {
		return err
	}
	res, err := conn.Exec(ctx, rebootScript, 30*time.Second)
	if err != nil {
		return nil // connection closed or no answer: the server is going down
	}
	if res.Code == 0 {
		return nil
	}
	msg := strings.TrimSpace(res.Stderr + res.Stdout)
	if strings.Contains(msg, "password is required") || strings.Contains(msg, "not in the sudoers") || strings.Contains(msg, "not allowed") {
		return errors.New("rebooting needs root: connect as root, or allow this user to run `systemctl reboot` with sudo without a password")
	}
	if msg == "" {
		msg = fmt.Sprintf("reboot failed (exit %d)", res.Code)
	}
	return errors.New(msg)
}
