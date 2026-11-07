// Package security reads and changes what decides who can get into a server: SSH sessions and logins,
// authorized SSH keys, the ufw firewall, and the ports reachable from the network (Docker's published
// ports skip ufw). Every command is built here from validated input; root is used when the SSH user is
// root or may run sudo without a password, otherwise the commands run as the user and see less.
package security

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Host runs a shell command on a server (over SSH in the app, in WSL in the integration tests).
// A command that exits non-zero returns an error with an ExitCode() int method.
type Host interface {
	Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error
}

type exitCoder interface{ ExitCode() int }

// Me is the SSH connection the app itself uses, as the server sees it.
type Me struct {
	User      string `json:"user"`
	IP        string `json:"ip"`        // this computer's address (or the jump host's)
	Port      int    `json:"port"`      // source port of the app's connection
	SSHPort   int    `json:"sshPort"`   // the server's SSH port
	SessionID string `json:"sessionId"` // logind session of the app's connection
	Root      bool   `json:"root"`      // commands ran as root (root user, or sudo without a password)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// wrap runs body with sh, as root when possible. The body gets $1 = 0 (root), 1 (sudo) or 2 (neither),
// $2 = SSH_CONNECTION and $3 = XDG_SESSION_ID of the app's session (sudo drops the environment), then args from $4.
// The first output line is "@@ME <R> <user> <SSH_CONNECTION> <XDG_SESSION_ID>".
func wrap(body string, args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = quote(a)
	}
	rest := strings.Join(q, " ")
	return `if [ "$(id -u)" -eq 0 ]; then R=0; elif sudo -n true 2>/dev/null; then R=1; else R=2; fi
printf '@@ME\t%s\t%s\t%s\t%s\n' "$R" "$(id -un)" "${SSH_CONNECTION:-}" "${XDG_SESSION_ID:-}"
B=` + quote(body) + `
if [ "$R" = 1 ]; then exec sudo -n sh -c "$B" sh "$R" "${SSH_CONNECTION:-}" "${XDG_SESSION_ID:-}" ` + rest + `
else exec sh -c "$B" sh "$R" "${SSH_CONNECTION:-}" "${XDG_SESSION_ID:-}" ` + rest + `
fi`
}

// run executes cmd; a non-zero exit is returned as code, not as an error.
func run(ctx context.Context, h Host, cmd string, stdin []byte, timeout time.Duration) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var o, e bytes.Buffer
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	err = h.Run(ctx, cmd, in, &o, &e)
	var ec exitCoder
	if errors.As(err, &ec) {
		return o.String(), e.String(), ec.ExitCode(), nil
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("the server did not answer within %v", timeout)
	}
	return o.String(), e.String(), 0, err
}

// failure turns a failed command into a plain-words error.
func failure(stdout, stderr string, code int, what string) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = strings.TrimSpace(stdout)
	}
	if i := strings.LastIndex(msg, "\n"); i >= 0 && len(msg) > 400 {
		msg = strings.TrimSpace(msg[i:])
	}
	switch {
	case strings.Contains(msg, "need to be root") || strings.Contains(msg, "Permission denied") ||
		strings.Contains(msg, "Operation not permitted") || strings.Contains(msg, "Access denied") ||
		strings.Contains(msg, "Interactive authentication required"):
		return fmt.Errorf("%s needs root: connect as root, or allow this user to use sudo without a password", what)
	case msg == "":
		return fmt.Errorf("%s failed (exit %d)", what, code)
	}
	return fmt.Errorf("%s failed: %s", what, msg)
}

// parseMe reads the "@@ME" line printed by wrap and returns the rest of the output.
func parseMe(out string) (Me, string) {
	line, rest, _ := strings.Cut(out, "\n")
	var me Me
	f := strings.Split(line, "\t")
	if len(f) < 5 || f[0] != "@@ME" {
		return me, out
	}
	me.Root = f[1] == "0" || f[1] == "1"
	me.User = f[2]
	if c := strings.Fields(f[3]); len(c) == 4 {
		me.IP = c[0]
		me.Port, _ = strconv.Atoi(c[1])
		me.SSHPort, _ = strconv.Atoi(c[3])
	}
	me.SessionID = f[4]
	return me, rest
}

// sections splits "@@NAME" marked output into the lines of each section.
func sections(out string) map[string][]string {
	m := map[string][]string{}
	cur := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "@@") {
			name, _, _ := strings.Cut(l[2:], " ")
			cur = name
			if _, ok := m[cur]; !ok {
				m[cur] = []string{}
			}
			continue
		}
		if cur != "" && l != "" {
			m[cur] = append(m[cur], l)
		}
	}
	return m
}
