package pgdb

// PostgreSQL installed on the server itself (apt, yum, a tarball…), not in a container.
//
// Such a server is named "native:<port>" where a container name would be, so the rest of the package works the
// same: its scripts run on the server as the postgres user, which logs in through the Unix socket without a
// password (peer authentication, the default of the distributions' packages), with pg_dump / pg_restore / psql
// of the server's own version (the folder of the running postgres binary). The SSH user needs to be postgres,
// root, or allowed "sudo -n -u postgres".
//
// The running servers are found from their processes: a postgres process outside any container whose pid is
// the first line of <data dir>/postmaster.pid, which also gives the port and the socket folder.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const nativePrefix = "native:"

var nativeRe = regexp.MustCompile(`^native:(\d{1,5})$`)

// nativePort returns the port of a native server's name ("native:5432"), or 0 for a container.
func nativePort(id string) int {
	m := nativeRe.FindStringSubmatch(id)
	if m == nil {
		return 0
	}
	p, _ := strconv.Atoi(m[1])
	if p <= 0 || p > 65535 {
		return 0
	}
	return p
}

// IsNative tells a native server's name from a container's.
func IsNative(id string) bool { return nativePort(id) > 0 }

// DisplayName is how messages name a container or a native server.
func DisplayName(id string) string {
	if p := nativePort(id); p > 0 {
		return fmt.Sprintf("the PostgreSQL installed on the server (port %d)", p)
	}
	return id
}

// sameFiles tells whether both ends of a copy see the same /tmp: the same container, or two native servers of
// one machine. The backup is then not transferred at all.
func sameFiles(s Spec) bool {
	if sameContainer(s) {
		return true
	}
	return s.Kind == "copy" && s.File == "" && s.Source.ServerID == s.Target.ServerID &&
		IsNative(s.Source.Container) && IsNative(s.Target.Container)
}

// postmasters prints the pids of the postgres processes that do not run in a container (any user may run it).
const postmasters = `ps -eo pid=,args= 2>/dev/null | awk '{ n = split($2, a, "/"); if (a[n] == "postgres" || a[n] == "postmaster") print $1 }' |
while read -r pid; do grep -qE 'docker|containerd|kubepods|libpod|lxc' /proc/$pid/cgroup 2>/dev/null || echo "$pid"; done`

// nativeScan defines _pgscan, which prints one line per native server: "<port> <bin dir> <data dir> <socket dir>".
// It runs as the postgres user (the data folder is private).
const nativeScan = `_pgscan() {
  ` + postmasters + ` | while read -r pid; do
    dd=$(tr '\0' '\n' < /proc/$pid/cmdline 2>/dev/null | awk 'p { print; exit } $0 == "-D" { p = 1 } /^-D./ { print substr($0, 3); exit }')
    [ -n "$dd" ] || dd=$(tr '\0' '\n' < /proc/$pid/environ 2>/dev/null | sed -n 's/^PGDATA=//p')
    [ -r "$dd/postmaster.pid" ] || continue
    [ "$(sed -n 1p "$dd/postmaster.pid")" = "$pid" ] || continue
    bin=$(readlink /proc/$pid/exe 2>/dev/null) || continue
    echo "$(sed -n 4p "$dd/postmaster.pid") ${bin%/*} $dd $(sed -n 5p "$dd/postmaster.pid")"
  done
}
`

// nativeEnv points the tools at the native server on a port: its binaries first on the PATH, its port and socket.
const nativeEnv = nativeScan + `_l=$(_pgscan | awk -v p=@PORT@ '$1 == p { print; exit }')
[ -n "$_l" ] || { echo "no PostgreSQL of this server listens on port @PORT@ any more" >&2; exit 3; }
set -- $_l
export PGPORT="$1" PGDATA="$3" PATH="$2:$PATH"
[ -n "$4" ] && export PGHOST="$4"
cd /tmp 2>/dev/null
`

// asPostgres runs a script as the postgres user of the server, with its stdin.
func asPostgres(script string) string {
	return "_s=" + quote(script) + `
if [ "$(id -un)" = postgres ]; then exec sh -c "$_s"
elif sudo -n -u postgres true 2>/dev/null; then exec sudo -n -u postgres sh -c "$_s"
elif [ "$(id -u)" = 0 ]; then exec su -s /bin/sh postgres -c "$_s"
else echo "the SSH user $(id -un) may not run commands as postgres: allow it in /etc/sudoers.d (` +
		`$(id -un) ALL=(postgres) NOPASSWD: ALL), or connect as root or postgres" >&2; exit 4
fi`
}

// nativeExec runs a script as the postgres user against the native server on port.
func nativeExec(port int, script string) string {
	return asPostgres(strings.ReplaceAll(nativeEnv, "@PORT@", strconv.Itoa(port)) + script)
}

// nativeError is the pseudo server listed when PostgreSQL runs on the machine but cannot be used.
const nativeError = "native"

// findNative lists the native servers of a machine as "native:<port>" names. When PostgreSQL runs but the SSH
// user may not act as postgres, it returns one instance carrying the reason.
func findNative(ctx context.Context, s Side) ([]Instance, error) {
	script := "[ -n \"$(" + postmasters + ")\" ] || exit 0\necho @@NATIVE\n" + asPostgres(nativeScan+"_pgscan") + " 2>&1"
	out, _ := run(ctx, s.Host, script, 30*time.Second)
	if !strings.Contains(out, "@@NATIVE") { // no PostgreSQL outside containers (a broken connection fails the Docker listing too)
		return nil, nil
	}
	_, out, _ = strings.Cut(out, "@@NATIVE\n")
	var list []Instance
	var other []string
	seen := map[int]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && strings.HasPrefix(f[1], "/") {
			if p, err := strconv.Atoi(f[0]); err == nil && p > 0 && p <= 65535 && !seen[p] {
				seen[p] = true
				list = append(list, Instance{Container: nativePrefix + f[0], Native: true, Port: p, DataDir: f[2]})
			}
			continue
		}
		if line = strings.TrimSpace(line); line != "" {
			other = append(other, line)
		}
	}
	if len(list) == 0 {
		msg := lastLines(strings.Join(other, "\n"), 3)
		if msg == "" {
			msg = "PostgreSQL runs on this server, but its port and data folder could not be read"
		}
		return []Instance{{Container: nativeError, Native: true, Error: capital(msg)}}, nil
	}
	return list, nil
}
