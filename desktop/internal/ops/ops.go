// Package ops runs the hands-on server operations: compose projects, Docker cleanup, systemd services,
// listening ports, disk usage by folder and TLS certificates. Every command is built here from validated
// input, so nothing the UI sends is pasted into a shell as-is.
package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"serverdash/internal/sshpool"
)

// Quote makes a string safe as one shell word.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// maybeRoot sets $S to "sudo -n" when the user is not root but may use sudo without a password, else "".
// Read-only commands use it to see everything when they can, and still work (partially) when they cannot.
const maybeRoot = `S=""; if [ "$(id -u)" -ne 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi; `

// asRoot runs a command as root: directly, or through passwordless sudo.
func asRoot(cmd string) string {
	return fmt.Sprintf(`if [ "$(id -u)" -eq 0 ]; then %s; else sudo -n %s; fi`, cmd, cmd)
}

// rootError explains a failed sudo in plain words.
func rootError(msg, what string) error {
	if strings.Contains(msg, "password is required") || strings.Contains(msg, "not in the sudoers") || strings.Contains(msg, "not allowed") {
		return fmt.Errorf("%s needs root: connect as root, or allow this user to use sudo without a password", what)
	}
	return errors.New(msg)
}

// run executes a command; a non-zero exit becomes an error with the command's own message.
func run(ctx context.Context, conn *sshpool.Conn, cmd, what string, timeout time.Duration) (string, error) {
	res, err := conn.Exec(ctx, cmd, timeout)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr + "\n" + res.Stdout)
		if msg == "" {
			msg = fmt.Sprintf("%s failed (exit %d)", what, res.Code)
		}
		return res.Stdout, rootError(msg, what)
	}
	return res.Stdout, nil
}

// validPath accepts absolute paths without control characters.
func validPath(p string) error {
	if !strings.HasPrefix(p, "/") || len(p) > 4096 || strings.ContainsAny(p, "\x00\n\r") {
		return errors.New("invalid path")
	}
	return nil
}

// banner ends a task run in a terminal with a clear result line.
func banner(script string) string {
	return script + `
rc=$?
echo
if [ $rc -eq 0 ]; then printf '\033[32m✔ Done\033[0m\n'; else printf '\033[31m✘ Failed (exit %s)\033[0m\n' "$rc"; fi
exit $rc`
}
