package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/crypto/ssh"

	"serverdash/internal/docker"
	"serverdash/internal/sshpool"
)

// OpenSSH allows 10 sessions per connection by default; metrics and log streams use some of them.
const maxTerminalsPerServer = 5

type terminal struct {
	server string
	term   *sshpool.Term
}

type terminals struct {
	mu   sync.Mutex
	list map[string]*terminal
}

func (t *terminals) countFor(server string) int {
	n := 0
	for _, x := range t.list {
		if x.server == server {
			n++
		}
	}
	return n
}

// OpenTerminal starts a shell on the server (container empty) or inside a container (docker exec),
// on the SSH connection the dashboard already holds. Output arrives as "term.data" events.
func (a *App) OpenTerminal(serverID, container string, cols, rows int) (string, error) {
	srv, ok := a.st.Get(serverID)
	if !ok {
		return "", errors.New("server not found")
	}

	command := ""
	if container != "" {
		if err := docker.ValidateName(container); err != nil {
			return "", err
		}
		// Prefer bash, fall back to sh (alpine/busybox images).
		command = fmt.Sprintf("exec %s exec -it -e TERM=xterm-256color %s sh -c 'if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi'",
			sshpool.DockerBin(srv), container)
	}
	return a.openPTY(serverID, command, cols, rows)
}

// openPTY runs command (the login shell when empty) in a pseudo-terminal and streams its output.
func (a *App) openPTY(serverID, command string, cols, rows int) (string, error) {
	a.terms.mu.Lock()
	if a.terms.countFor(serverID) >= maxTerminalsPerServer {
		a.terms.mu.Unlock()
		return "", fmt.Errorf("at most %d terminals per server: close one first", maxTerminalsPerServer)
	}
	a.terms.mu.Unlock()
	conn, err := a.pool.Get(serverID)
	if err != nil {
		return "", err
	}
	term, err := conn.Terminal(max(cols, 20), max(rows, 5), command)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	id := hex.EncodeToString(raw)
	a.terms.mu.Lock()
	a.terms.list[id] = &terminal{server: serverID, term: term}
	a.terms.mu.Unlock()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				// Base64 keeps multi-byte UTF-8 characters split across reads intact.
				runtime.EventsEmit(a.ctx, "term.data", map[string]string{"id": id, "data": base64.StdEncoding.EncodeToString(buf[:n])})
			}
			if err != nil {
				break
			}
		}
		reason := "session ended"
		code := 0
		if werr := term.Wait(); werr != nil && !strings.Contains(werr.Error(), "exited without exit status") {
			reason = werr.Error()
			var exit *ssh.ExitError
			if errors.As(werr, &exit) {
				code = exit.ExitStatus()
				reason = fmt.Sprintf("exit code %d", code)
			}
		}
		a.terms.mu.Lock()
		delete(a.terms.list, id)
		a.terms.mu.Unlock()
		runtime.EventsEmit(a.ctx, "term.exit", map[string]any{"id": id, "reason": reason, "code": code})
	}()
	return id, nil
}

func (a *App) termByID(id string) *terminal {
	a.terms.mu.Lock()
	defer a.terms.mu.Unlock()
	return a.terms.list[id]
}

// TermInput sends keystrokes or pasted text to a terminal.
func (a *App) TermInput(id, data string) error {
	t := a.termByID(id)
	if t == nil {
		return errors.New("terminal closed")
	}
	_, err := t.term.Write([]byte(data))
	return err
}

func (a *App) TermResize(id string, cols, rows int) error {
	t := a.termByID(id)
	if t == nil || cols < 1 || rows < 1 {
		return nil
	}
	return t.term.Resize(cols, rows)
}

func (a *App) CloseTerminal(id string) {
	if t := a.termByID(id); t != nil {
		t.term.Close()
	}
}

func (a *App) closeAllTerminals() {
	a.terms.mu.Lock()
	defer a.terms.mu.Unlock()
	for _, t := range a.terms.list {
		t.term.Close()
	}
}
