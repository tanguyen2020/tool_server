package ops

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
	"serverdash/internal/store"
)

// ------------------------------------------------------------------ Docker cleanup

var pruneArgs = map[string]string{
	"containers":      "container prune -f",
	"images-dangling": "image prune -f",
	"images-unused":   "image prune -a -f",
	"build-cache":     "builder prune -f",
	"volumes":         "volume prune -f",
	"networks":        "network prune -f",
}

// Prune frees Docker disk space. Returns docker's summary ("Total reclaimed space: 1.2GB").
func Prune(ctx context.Context, conn *sshpool.Conn, srv store.Server, kind string) (string, error) {
	args, ok := pruneArgs[kind]
	if !ok {
		return "", errors.New("invalid cleanup type")
	}
	out, err := run(ctx, conn, sshpool.DockerBin(srv)+" "+args, "docker "+strings.Fields(args)[0]+" prune", 10*time.Minute)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Total reclaimed space") {
			return l, nil
		}
	}
	if out == "" {
		return "Nothing to remove", nil
	}
	return out, nil
}

// ------------------------------------------------------------------ systemd services

type Service struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Load        string `json:"load"`    // loaded, not-found, masked…
	Active      string `json:"active"`  // active, inactive, failed…
	Sub         string `json:"sub"`     // running, exited, dead…
	Enabled     string `json:"enabled"` // enabled, disabled, static, masked…
}

var serviceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._:\\-]{0,200}\.service$`)

func ValidService(name string) error {
	if !serviceRe.MatchString(name) {
		return errors.New("invalid service name")
	}
	return nil
}

const servicesScript = `export LC_ALL=C
systemctl list-units --type=service --all --no-pager --no-legend --plain 2>/dev/null
echo @@FILES
systemctl list-unit-files --type=service --no-pager --no-legend 2>/dev/null`

func parseServices(out string) []Service {
	units, files, _ := strings.Cut(out, "@@FILES")
	byName := map[string]*Service{}
	var order []string
	for _, l := range strings.Split(units, "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "● "))
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		s := &Service{Name: f[0], Load: f[1], Active: f[2], Sub: f[3]}
		if len(f) > 4 {
			s.Description = strings.Join(f[4:], " ")
		}
		byName[s.Name] = s
		order = append(order, s.Name)
	}
	for _, l := range strings.Split(files, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || !strings.HasSuffix(f[0], ".service") || strings.HasSuffix(f[0], "@.service") {
			continue // templates (foo@.service) are not services by themselves
		}
		if s, ok := byName[f[0]]; ok {
			s.Enabled = f[1]
			continue
		}
		byName[f[0]] = &Service{Name: f[0], Load: "not loaded", Active: "inactive", Sub: "dead", Enabled: f[1]}
		order = append(order, f[0])
	}
	out2 := make([]Service, 0, len(order))
	for _, n := range order {
		out2 = append(out2, *byName[n])
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].Name < out2[j].Name })
	return out2
}

func Services(ctx context.Context, conn *sshpool.Conn) ([]Service, error) {
	out, err := run(ctx, conn, servicesScript, "systemctl", 30*time.Second)
	if err != nil {
		return nil, err
	}
	list := parseServices(out)
	if len(list) == 0 {
		return nil, errors.New("no systemd services found (is this a systemd system?)")
	}
	return list, nil
}

var serviceActions = map[string]bool{"start": true, "stop": true, "restart": true, "reload": true, "enable": true, "disable": true}

func ServiceAction(ctx context.Context, conn *sshpool.Conn, name, action string) error {
	if err := ValidService(name); err != nil {
		return err
	}
	if !serviceActions[action] {
		return errors.New("invalid action")
	}
	_, err := run(ctx, conn, asRoot("systemctl "+action+" "+Quote(name)), "systemctl "+action, 90*time.Second)
	return err
}

// ServiceStatus is the `systemctl status` text (last log lines included).
func ServiceStatus(ctx context.Context, conn *sshpool.Conn, name string) (string, error) {
	if err := ValidService(name); err != nil {
		return "", err
	}
	// status exits 3 for a stopped service: that is not an error here.
	res, err := conn.Exec(ctx, maybeRoot+"LC_ALL=C $S systemctl status --no-pager -l -n 30 "+Quote(name)+" 2>&1", 30*time.Second)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// JournalScript follows a service's journal in a terminal.
func JournalScript(name string) (string, error) {
	if err := ValidService(name); err != nil {
		return "", err
	}
	return maybeRoot + "exec $S journalctl -u " + Quote(name) + " -f -n 300 --no-pager", nil
}

// ------------------------------------------------------------------ listening ports

type Port struct {
	Proto   string   `json:"proto"` // tcp, udp
	Port    int      `json:"port"`
	Addrs   []string `json:"addrs"` // 0.0.0.0, [::], 127.0.0.1…
	Process string   `json:"process"`
	PID     int      `json:"pid"`
}

var procRe = regexp.MustCompile(`\(\("([^"]+)",pid=(\d+)`)

func parsePorts(out string) []Port {
	type key struct {
		proto string
		port  int
		proc  string
	}
	merged := map[key]*Port{}
	var order []key
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		proto := f[0]
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if proto == "tcp" && f[1] != "LISTEN" {
			continue
		}
		local := f[4]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		addr := local[:i]
		if j := strings.Index(addr, "%"); j >= 0 {
			addr = addr[:j] // 127.0.0.53%lo
		}
		p := Port{Proto: proto, Port: port}
		if m := procRe.FindStringSubmatch(l); m != nil {
			p.Process = m[1]
			p.PID, _ = strconv.Atoi(m[2])
		}
		k := key{proto, port, p.Process}
		if x, ok := merged[k]; ok {
			if !contains(x.Addrs, addr) {
				x.Addrs = append(x.Addrs, addr)
			}
			continue
		}
		p.Addrs = []string{addr}
		merged[k] = &p
		order = append(order, k)
	}
	out2 := make([]Port, 0, len(order))
	for _, k := range order {
		out2 = append(out2, *merged[k])
	}
	sort.Slice(out2, func(i, j int) bool {
		if out2[i].Port != out2[j].Port {
			return out2[i].Port < out2[j].Port
		}
		return out2[i].Proto < out2[j].Proto
	})
	return out2
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ListeningPorts lists TCP/UDP ports the server listens on (process names need root: shown when available).
func ListeningPorts(ctx context.Context, conn *sshpool.Conn) ([]Port, bool, error) {
	out, err := run(ctx, conn, maybeRoot+`echo "@@ROOT:${S:-$(id -u)}"; $S ss -H -tulnp 2>/dev/null || ss -H -tuln`, "ss", 30*time.Second)
	if err != nil {
		return nil, false, err
	}
	withProcs := strings.Contains(out, "@@ROOT:sudo") || strings.Contains(out, "@@ROOT:0")
	return parsePorts(out), withProcs, nil
}

// ------------------------------------------------------------------ disk usage by folder

type DirSize struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// DiskUsageAt lists the sub-folders of path by size (du on one filesystem; root rights used when possible).
func DiskUsageAt(ctx context.Context, conn *sshpool.Conn, path string) (total int64, dirs []DirSize, err error) {
	if err := validPath(path); err != nil {
		return 0, nil, err
	}
	// Low CPU and disk priority: scanning a big disk must not slow down what runs on the server.
	cmd := maybeRoot + `N=""; command -v ionice >/dev/null 2>&1 && N="ionice -c3"; ` +
		"$S nice -n 19 $N du -x -B1 -d1 -- " + Quote(path) + " 2>/dev/null | sort -rn | head -n 41"
	res, err := conn.Exec(ctx, cmd, 5*time.Minute)
	if err != nil && strings.Contains(err.Error(), "timed out") {
		return 0, nil, fmt.Errorf("measuring %s took more than 5 minutes: pick a sub-folder (e.g. /var) to measure", path)
	}
	if err != nil {
		return 0, nil, err
	}
	dirs = []DirSize{}
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		size, p, ok := strings.Cut(l, "\t")
		n, perr := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
		if !ok || perr != nil {
			continue
		}
		if p == path || p == strings.TrimSuffix(path, "/") || (path == "/" && p == "/") {
			total = n
			continue
		}
		dirs = append(dirs, DirSize{Path: p, Size: n})
	}
	if total == 0 && len(dirs) == 0 {
		return 0, nil, fmt.Errorf("cannot read %s (missing or no permission)", path)
	}
	return total, dirs, nil
}

// ------------------------------------------------------------------ package upgrade

// UpgradeScript runs apt in a terminal: the user sees every prompt and answers it (and types a sudo password if asked).
func UpgradeScript() string {
	return banner(`S=""; [ "$(id -u)" -ne 0 ] && S="sudo"
export DEBIAN_FRONTEND=readline
$S apt-get update && $S apt-get upgrade`)
}

// ShellAt opens a login shell in a folder.
func ShellAt(path string) (string, error) {
	if err := validPath(path); err != nil {
		return "", err
	}
	return "cd " + Quote(path) + ` && exec "${SHELL:-/bin/sh}" -l`, nil
}
