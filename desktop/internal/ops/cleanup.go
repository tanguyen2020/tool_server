package ops

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// needRoot stops a script with @@NOROOT when it can run neither as root nor through passwordless sudo.
const needRoot = maybeRoot + `if [ "$(id -u)" -ne 0 ] && [ -z "$S" ]; then echo @@NOROOT; exit 0; fi; `

var errNoRoot = errors.New("needs root: connect as root, or allow this user to use sudo without a password")

// runRoot runs a needRoot script and turns @@NOROOT into errNoRoot.
func runRoot(ctx context.Context, conn *sshpool.Conn, script, what string, timeout time.Duration) (string, error) {
	out, err := run(ctx, conn, needRoot+script, what, timeout)
	if err == nil && strings.HasPrefix(strings.TrimSpace(out), "@@NOROOT") {
		return "", errNoRoot
	}
	return out, err
}

// ------------------------------------------------------------------ system cleanup

// CleanupItem is one kind of leftover the server can do without, and the space it takes.
type CleanupItem struct {
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"` // -1 when unknown
	Count int    `json:"count"` // files or packages; -1 when unknown
}

// SystemCleanupInfo reports the space each cleanup would free and whether we may act as root.
type SystemCleanupInfo struct {
	Root  bool          `json:"root"`
	Items []CleanupItem `json:"items"`
}

// What each cleanup measures (prints "<kind> <bytes> <count>") and removes. Paths are fixed: nothing comes from the UI.
const (
	rotatedFind = `/var/log -xdev -type f \( -name '*.gz' -o -name '*.xz' -o -name '*.bz2' -o -name '*.[0-9]' -o -name '*.[0-9].log' -o -name '*.old' \)`
	coreFind    = `/var/lib/systemd/coredump /var/crash -xdev -type f`
	tmpFind     = `/tmp /var/tmp -xdev -type f -mtime +10`
	sumFiles    = ` -printf '%s\n' 2>/dev/null | awk -v k=KIND '{s+=$1; n++} END {print k, s+0, n+0}'`
)

var measureScripts = map[string]string{
	"journal":      `j=$($S du -scb /var/log/journal /run/log/journal 2>/dev/null | tail -n1 | cut -f1); echo journal ${j:-0} -1`,
	"apt-cache":    `a=$($S du -sb /var/cache/apt/archives 2>/dev/null | cut -f1); n=$(ls /var/cache/apt/archives/*.deb 2>/dev/null | wc -l); echo apt-cache ${a:-0} $n`,
	"autoremove":   `n=$(LC_ALL=C apt-get -s autoremove 2>/dev/null | grep -c '^Remv'); echo autoremove -1 ${n:-0}`,
	"rotated-logs": `$S find ` + rotatedFind + strings.Replace(sumFiles, "KIND", "rotated-logs", 1),
	"coredumps":    `$S find ` + coreFind + strings.Replace(sumFiles, "KIND", "coredumps", 1),
	"tmp":          `$S find ` + tmpFind + strings.Replace(sumFiles, "KIND", "tmp", 1),
}

var cleanScripts = map[string]string{
	// Rotate first so the active files become archives vacuum may delete. Keeps 7 days, at most 500 MB.
	"journal":      `$S journalctl --rotate >/dev/null 2>&1; $S journalctl --vacuum-time=7d --vacuum-size=500M`,
	"apt-cache":    `$S apt-get clean`,
	"rotated-logs": `$S find ` + rotatedFind + ` -delete`,
	"coredumps":    `$S find ` + coreFind + ` -delete`,
	"tmp":          `$S find ` + tmpFind + ` -delete`,
}

// cleanupOrder is the order the items are shown in.
var cleanupOrder = []string{"journal", "apt-cache", "autoremove", "rotated-logs", "coredumps", "tmp"}

// SystemCleanup measures every cleanup kind. Without root the sizes only cover what this user can read.
func SystemCleanup(ctx context.Context, conn *sshpool.Conn) (SystemCleanupInfo, error) {
	var sb strings.Builder
	sb.WriteString(maybeRoot + `echo "@@ROOT:${S:-$(id -u)}"` + "\n")
	for _, k := range cleanupOrder {
		sb.WriteString(measureScripts[k] + "\n")
	}
	out, err := run(ctx, conn, sb.String()+"exit 0", "measure cleanup", 3*time.Minute)
	if err != nil {
		return SystemCleanupInfo{}, err
	}
	return parseSystemCleanup(out), nil
}

func parseSystemCleanup(out string) SystemCleanupInfo {
	info := SystemCleanupInfo{Root: strings.Contains(out, "@@ROOT:sudo") || strings.Contains(out, "@@ROOT:0"), Items: []CleanupItem{}}
	got := map[string]CleanupItem{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		b, err1 := strconv.ParseInt(f[1], 10, 64)
		n, err2 := strconv.Atoi(f[2])
		if err1 == nil && err2 == nil {
			got[f[0]] = CleanupItem{Kind: f[0], Bytes: b, Count: n}
		}
	}
	for _, k := range cleanupOrder {
		if it, ok := got[k]; ok {
			info.Items = append(info.Items, it)
		} else {
			info.Items = append(info.Items, CleanupItem{Kind: k, Bytes: -1, Count: -1})
		}
	}
	return info
}

// RunSystemCleanup removes one kind of leftovers (as root) and measures again.
// apt autoremove is not here: it runs in a terminal (AutoremoveScript) so its prompt is seen.
func RunSystemCleanup(ctx context.Context, conn *sshpool.Conn, kind string) (SystemCleanupInfo, error) {
	script, ok := cleanScripts[kind]
	if !ok {
		return SystemCleanupInfo{}, errors.New("invalid cleanup type")
	}
	if _, err := runRoot(ctx, conn, script, "clean up "+kind, 10*time.Minute); err != nil {
		return SystemCleanupInfo{}, err
	}
	return SystemCleanup(ctx, conn)
}

// AutoremoveScript removes packages nothing depends on any more (old kernels…) in a terminal.
func AutoremoveScript() string {
	return banner(`S=""; [ "$(id -u)" -ne 0 ] && S="sudo"
export DEBIAN_FRONTEND=readline
$S apt-get autoremove`)
}

// ------------------------------------------------------------------ container log files

// ContainerLog is a container's log file. Docker keeps json-file logs forever unless max-size is set.
type ContainerLog struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Driver  string `json:"driver"`
	MaxSize string `json:"maxSize"` // "" = no limit
	MaxFile string `json:"maxFile"`
	Path    string `json:"path"`
	Size    int64  `json:"size"` // -1 when unknown (no root, or not a json-file log)
}

type ContainerLogs struct {
	Root bool           `json:"root"`
	Logs []ContainerLog `json:"logs"`
}

var (
	containerIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	logPathRe     = regexp.MustCompile(`^/[A-Za-z0-9/_.-]+-json\.log$`)
)

// {{"\t"}}: inspect templates do not turn a plain \t into a tab, unlike the --format of the ls commands.
const logInspect = `'{{.Id}}{{"\t"}}{{.Name}}{{"\t"}}{{.State.Running}}{{"\t"}}{{.LogPath}}{{"\t"}}{{json .HostConfig.LogConfig}}'`

// FindContainerLogs lists every container's log file with its size, largest first.
func FindContainerLogs(ctx context.Context, conn *sshpool.Conn, srv store.Server) (ContainerLogs, error) {
	d := sshpool.DockerBin(srv)
	script := maybeRoot + fmt.Sprintf(`echo "@@ROOT:${S:-$(id -u)}"
echo @@INSPECT; %[1]s ps -aq --no-trunc | xargs -r %[1]s container inspect -f %[2]s 2>/dev/null
echo @@SIZES; %[1]s ps -aq --no-trunc | xargs -r %[1]s container inspect -f '{{.LogPath}}' 2>/dev/null | grep . | xargs -r $S stat -c '%%s %%n' -- 2>/dev/null
exit 0`, d, logInspect)
	out, err := run(ctx, conn, script, "list container logs", 2*time.Minute)
	if err != nil {
		return ContainerLogs{}, err
	}
	return parseContainerLogs(out), nil
}

func parseContainerLogs(out string) ContainerLogs {
	res := ContainerLogs{Root: strings.Contains(out, "@@ROOT:sudo") || strings.Contains(out, "@@ROOT:0"), Logs: []ContainerLog{}}
	sizes := map[string]int64{}
	sec := ""
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.HasPrefix(l, "@@"):
			sec = l
		case sec == "@@INSPECT":
			rows = append(rows, l)
		case sec == "@@SIZES":
			if s, p, ok := strings.Cut(l, " "); ok {
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					sizes[p] = n
				}
			}
		}
	}
	for _, l := range rows {
		f := strings.SplitN(l, "\t", 5)
		if len(f) < 5 || !containerIDRe.MatchString(f[0]) {
			continue
		}
		var lc struct {
			Type   string
			Config map[string]string
		}
		_ = json.Unmarshal([]byte(f[4]), &lc)
		c := ContainerLog{ID: f[0], Name: strings.TrimPrefix(f[1], "/"), Running: f[2] == "true", Driver: lc.Type,
			MaxSize: lc.Config["max-size"], MaxFile: lc.Config["max-file"], Path: f[3], Size: -1}
		if n, ok := sizes[f[3]]; ok && f[3] != "" {
			c.Size = n
		}
		res.Logs = append(res.Logs, c)
	}
	sort.SliceStable(res.Logs, func(a, b int) bool { return res.Logs[a].Size > res.Logs[b].Size })
	return res
}

// TruncateLogs empties the log files of the given containers. Docker appends to the file, so a running
// container keeps logging into the emptied file. Returns the bytes freed.
func TruncateLogs(ctx context.Context, conn *sshpool.Conn, srv store.Server, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, errors.New("nothing selected")
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		if !containerIDRe.MatchString(id) {
			return 0, errors.New("invalid container ID")
		}
		quoted[i] = Quote(id)
	}
	// The paths come from docker itself, never from the UI.
	out, err := run(ctx, conn, sshpool.DockerBin(srv)+" container inspect -f '{{.LogPath}}' "+strings.Join(quoted, " "), "inspect", time.Minute)
	if err != nil {
		return 0, err
	}
	var sb strings.Builder
	n := 0
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		p = strings.TrimSpace(p)
		if !logPathRe.MatchString(p) || strings.Contains(p, "..") {
			continue // not a json-file log
		}
		q := Quote(p)
		sb.WriteString(`s=$($S stat -c %s -- ` + q + ` 2>/dev/null || echo 0); $S truncate -s 0 -- ` + q + ` && echo "@@FREED $s"` + "\n")
		n++
	}
	if n == 0 {
		return 0, errors.New("these containers do not log to a json-file log that can be emptied")
	}
	out, err = runRoot(ctx, conn, sb.String(), "empty container logs", 2*time.Minute)
	var freed int64
	for _, l := range strings.Split(out, "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(l), "@@FREED "); ok {
			v, _ := strconv.ParseInt(s, 10, 64)
			freed += v
		}
	}
	return freed, err
}

// ------------------------------------------------------------------ default log limit (daemon.json)

const daemonJSON = "/etc/docker/daemon.json"

// DaemonLogConfig is the logging docker uses for new containers.
type DaemonLogConfig struct {
	Driver  string `json:"driver"` // docker info: the driver new containers get
	MaxSize string `json:"maxSize"`
	MaxFile string `json:"maxFile"`
	Exists  bool   `json:"exists"` // daemon.json exists
}

func readDaemonJSON(ctx context.Context, conn *sshpool.Conn, srv store.Server) (map[string]any, bool, string, error) {
	out, err := run(ctx, conn, maybeRoot+sshpool.DockerBin(srv)+` info --format '@@DRIVER {{.LoggingDriver}}'; echo; `+
		`if [ -e `+daemonJSON+` ]; then echo @@FILE; $S cat `+daemonJSON+`; fi`, "read daemon.json", 30*time.Second)
	if err != nil {
		return nil, false, "", err
	}
	driver := ""
	head, body, exists := strings.Cut(out, "@@FILE\n")
	for _, l := range strings.Split(head, "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(l), "@@DRIVER "); ok {
			driver = s
		}
	}
	cfg := map[string]any{}
	if exists && strings.TrimSpace(body) != "" {
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			return nil, true, driver, fmt.Errorf("%s is not valid JSON: %v", daemonJSON, err)
		}
	}
	return cfg, exists, driver, nil
}

func GetDaemonLogConfig(ctx context.Context, conn *sshpool.Conn, srv store.Server) (DaemonLogConfig, error) {
	cfg, exists, driver, err := readDaemonJSON(ctx, conn, srv)
	if err != nil {
		return DaemonLogConfig{}, err
	}
	c := DaemonLogConfig{Driver: driver, Exists: exists}
	if opts, ok := cfg["log-opts"].(map[string]any); ok {
		c.MaxSize, _ = opts["max-size"].(string)
		c.MaxFile, _ = opts["max-file"].(string)
	}
	return c, nil
}

var (
	maxSizeRe = regexp.MustCompile(`^[1-9][0-9]{0,5}[kmg]$`)
	maxFileRe = regexp.MustCompile(`^[1-9][0-9]?$`)
)

// SetDaemonLogLimit sets log-opts max-size / max-file in daemon.json, keeping every other setting.
// The old file is kept as daemon.json.bak, and the new one is checked with `dockerd --validate` when
// that exists. Docker reads it when it restarts; only containers created after that get the limit.
func SetDaemonLogLimit(ctx context.Context, conn *sshpool.Conn, srv store.Server, maxSize, maxFile string) error {
	if !maxSizeRe.MatchString(maxSize) {
		return errors.New("max size: a number followed by k, m or g (e.g. 50m)")
	}
	if !maxFileRe.MatchString(maxFile) {
		return errors.New("max files: a number from 1 to 99")
	}
	cfg, _, driver, err := readDaemonJSON(ctx, conn, srv)
	if err != nil {
		return err
	}
	if driver == "" {
		return errors.New("could not read docker's log driver (docker info failed)")
	}
	if driver != "json-file" && driver != "local" {
		return fmt.Errorf("docker uses the %q log driver here: max-size only applies to json-file and local", driver)
	}
	opts, _ := cfg["log-opts"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	opts["max-size"] = maxSize
	opts["max-file"] = maxFile
	cfg["log-opts"] = opts
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(append(data, '\n'))
	f := daemonJSON
	script := `set -e; $S mkdir -p /etc/docker
[ -e ` + f + ` ] && $S cp -p ` + f + ` ` + f + `.bak
printf '%s' '` + b64 + `' | base64 -d | $S tee ` + f + `.new >/dev/null
if command -v dockerd >/dev/null 2>&1 && dockerd --help 2>&1 | grep -q -- '--validate'; then
  if ! out=$($S dockerd --validate --config-file=` + f + `.new 2>&1); then $S rm -f ` + f + `.new; echo "$out" >&2; exit 1; fi
fi
$S mv ` + f + `.new ` + f
	_, err = runRoot(ctx, conn, script, "write daemon.json", time.Minute)
	return err
}
