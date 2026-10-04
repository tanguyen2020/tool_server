package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"serverdash/internal/activity"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

func newToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ------------------------------------------------------------------ port forwarding

// TunnelInfo is a local port forwarded to a host:port reachable from a server (like ssh -L).
type TunnelInfo struct {
	ID         string `json:"id"`
	ServerID   string `json:"serverId"`
	Server     string `json:"server"`
	LocalPort  int    `json:"localPort"`
	RemoteHost string `json:"remoteHost"`
	RemotePort int    `json:"remotePort"`
	Active     int32  `json:"active"` // open connections
	Since      int64  `json:"since"`
	LastError  string `json:"lastError,omitempty"`
}

type tunnel struct {
	info   TunnelInfo
	ln     net.Listener
	active atomic.Int32
	mu     sync.Mutex
	errMsg string
	conns  map[net.Conn]bool // open local connections, closed when the forward stops
}

type tunnels struct {
	mu   sync.Mutex
	list map[string]*tunnel
}

var hostRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$|^\[?[0-9a-fA-F:]+\]?$`)

func (a *App) emitTunnels() { runtime.EventsEmit(a.ctx, "tunnels", a.ListTunnels()) }

// StartTunnel listens on 127.0.0.1:localPort (0 = any free port) and forwards each connection
// through the server's SSH connection to remoteHost:remotePort.
func (a *App) StartTunnel(serverID string, localPort int, remoteHost string, remotePort int) (TunnelInfo, error) {
	srv, conn, err := a.server(serverID)
	if err != nil {
		return TunnelInfo{}, err
	}
	remoteHost = strings.TrimSpace(remoteHost)
	if remoteHost == "" {
		remoteHost = "127.0.0.1"
	}
	if !hostRe.MatchString(remoteHost) {
		return TunnelInfo{}, errors.New("invalid remote host")
	}
	if localPort < 0 || localPort > 65535 || remotePort < 1 || remotePort > 65535 {
		return TunnelInfo{}, errors.New("ports must be between 1 and 65535")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return TunnelInfo{}, fmt.Errorf("port %d is already in use on this computer", localPort)
	}
	t := &tunnel{ln: ln, conns: map[net.Conn]bool{}, info: TunnelInfo{
		ID: newToken(), ServerID: serverID, Server: srv.Name, LocalPort: ln.Addr().(*net.TCPAddr).Port,
		RemoteHost: strings.Trim(remoteHost, "[]"), RemotePort: remotePort, Since: time.Now().UnixMilli(),
	}}
	target := net.JoinHostPort(t.info.RemoteHost, strconv.Itoa(remotePort))
	a.tun.mu.Lock()
	a.tun.list[t.info.ID] = t
	a.tun.mu.Unlock()
	go func() {
		for {
			local, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			t.mu.Lock()
			t.conns[local] = true
			t.mu.Unlock()
			go func() {
				defer func() {
					local.Close()
					t.mu.Lock()
					delete(t.conns, local)
					t.mu.Unlock()
				}()
				remote, err := conn.Dial(target)
				if err != nil {
					t.mu.Lock()
					t.errMsg = fmt.Sprintf("%s: %v", time.Now().Format("15:04:05"), err)
					t.mu.Unlock()
					a.emitTunnels()
					return
				}
				defer remote.Close()
				t.active.Add(1)
				a.emitTunnels()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
				go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
				<-done
				t.active.Add(-1)
				a.emitTunnels()
			}()
		}
	}()
	a.audit(serverID, "tunnel start", fmt.Sprintf("localhost:%d → %s", t.info.LocalPort, target), nil)
	a.emitTunnels()
	return t.snapshot(), nil
}

func (t *tunnel) snapshot() TunnelInfo {
	info := t.info
	info.Active = t.active.Load()
	t.mu.Lock()
	info.LastError = t.errMsg
	t.mu.Unlock()
	return info
}

func (a *App) ListTunnels() []TunnelInfo {
	a.tun.mu.Lock()
	defer a.tun.mu.Unlock()
	out := make([]TunnelInfo, 0, len(a.tun.list))
	for _, t := range a.tun.list {
		out = append(out, t.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since < out[j].Since })
	return out
}

func (a *App) StopTunnel(id string) {
	a.tun.mu.Lock()
	t := a.tun.list[id]
	delete(a.tun.list, id)
	a.tun.mu.Unlock()
	if t != nil {
		_ = t.ln.Close()
		t.mu.Lock()
		for c := range t.conns {
			_ = c.Close()
		}
		t.mu.Unlock()
		a.audit(t.info.ServerID, "tunnel stop", fmt.Sprintf("localhost:%d", t.info.LocalPort), nil)
		a.emitTunnels()
	}
}

func (a *App) stopTunnels(serverID string) {
	for _, t := range a.ListTunnels() {
		if serverID == "" || t.ServerID == serverID {
			a.StopTunnel(t.ID)
		}
	}
}

// ------------------------------------------------------------------ one command on many servers

type RunResult struct {
	RunID    string `json:"runId"`
	ServerID string `json:"serverId"`
	Code     int    `json:"code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
	Ms       int64  `json:"ms"`
}

type runs struct {
	mu   sync.Mutex
	list map[string]context.CancelFunc
}

const maxRunOutput = 100 * 1024

func clip(s string) string {
	if len(s) > maxRunOutput {
		return s[:maxRunOutput] + "\n… (output cut at 100 KB)"
	}
	return s
}

// RunMany runs one command on several servers at once (8 in parallel), without a terminal.
// Each result arrives as a "batch.result" event, then "batch.done".
func (a *App) RunMany(serverIDs []string, command string, timeoutSec int) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("enter a command")
	}
	if len(command) > 20000 {
		return "", errors.New("the command is too long")
	}
	if len(serverIDs) == 0 {
		return "", errors.New("select at least one server")
	}
	if timeoutSec < 5 || timeoutSec > 3600 {
		timeoutSec = 120
	}
	runID := newToken()
	ctx, cancel := context.WithCancel(a.ctx)
	a.runs.mu.Lock()
	a.runs.list[runID] = cancel
	a.runs.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			a.runs.mu.Lock()
			delete(a.runs.list, runID)
			a.runs.mu.Unlock()
			runtime.EventsEmit(a.ctx, "batch.done", runID)
		}()
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		for _, id := range serverIDs {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				start := time.Now()
				r := RunResult{RunID: runID, ServerID: id, Code: -1}
				_, conn, err := a.server(id)
				if err == nil {
					var out sshpool.Result
					out, err = conn.Exec(ctx, command, time.Duration(timeoutSec)*time.Second)
					r.Stdout, r.Stderr, r.Code = clip(out.Stdout), clip(out.Stderr), out.Code
				}
				if err != nil {
					r.Error = err.Error()
					if errors.Is(err, context.Canceled) {
						r.Error = "cancelled"
					}
				}
				r.Ms = time.Since(start).Milliseconds()
				var aerr error
				if r.Error != "" {
					aerr = errors.New(r.Error)
				} else if r.Code != 0 {
					aerr = fmt.Errorf("exit code %d", r.Code)
				}
				a.audit(id, "run command", firstLine(command), aerr)
				runtime.EventsEmit(a.ctx, "batch.result", r)
			}(id)
		}
		wg.Wait()
	}()
	return runID, nil
}

func firstLine(s string) string {
	l, _, more := strings.Cut(s, "\n")
	if len(l) > 120 {
		l = l[:120] + "…"
	} else if more {
		l += " …"
	}
	return l
}

func (a *App) CancelRun(runID string) {
	a.runs.mu.Lock()
	cancel := a.runs.list[runID]
	a.runs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ------------------------------------------------------------------ snippets & activity

func (a *App) Snippets() []store.Snippet { return a.st.Snippets() }

func (a *App) SaveSnippet(s store.Snippet) (store.Snippet, error) { return a.st.SaveSnippet(s) }

func (a *App) DeleteSnippet(id string) error { return a.st.DeleteSnippet(id) }

func (a *App) Activity(serverID string, limit int) []activity.Entry {
	if a.act == nil {
		return []activity.Entry{}
	}
	if limit < 1 || limit > 5000 {
		limit = 500
	}
	return a.act.List(serverID, limit)
}

// ------------------------------------------------------------------ export / import

type exportServer struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Group          string `json:"group,omitempty"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	AuthType       string `json:"authType"`
	PrivateKeyPath string `json:"privateKeyPath,omitempty"`
	UseSudo        bool   `json:"useSudo"`
	JumpID         string `json:"jumpId,omitempty"`
}

type exportFile struct {
	Format     string          `json:"format"`
	ExportedAt string          `json:"exportedAt"`
	Note       string          `json:"note"`
	Servers    []exportServer  `json:"servers"`
	Snippets   []store.Snippet `json:"snippets"`
}

const exportFormat = "serverdash/1"

// ExportServers writes the server list and snippets to a JSON file, without any password or key.
func (a *App) ExportServers() (string, error) {
	file, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Export servers", DefaultFilename: "serverdash-servers.json",
		Filters: []runtime.FileFilter{{DisplayName: "JSON (*.json)", Pattern: "*.json"}},
	})
	if err != nil || file == "" {
		return "", err
	}
	out := exportFile{Format: exportFormat, ExportedAt: time.Now().Format(time.RFC3339),
		Note: "No passwords or private keys are included: enter them again after importing.", Snippets: a.st.Snippets()}
	for _, s := range a.st.List() {
		out.Servers = append(out.Servers, exportServer{s.ID, s.Name, s.Group, s.Host, s.Port, s.Username, s.AuthType, s.PrivateKeyPath, s.UseSudo, s.JumpID})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return "", err
	}
	a.audit("", "export servers", file, nil)
	return file, nil
}

type ImportResult struct {
	Added        []string `json:"added"`
	Skipped      []string `json:"skipped"`      // already in the list
	NeedsSecrets []string `json:"needsSecrets"` // password / pasted key to enter again
	Snippets     int      `json:"snippets"`
}

func (a *App) ImportServers() (ImportResult, error) {
	res := ImportResult{Added: []string{}, Skipped: []string{}, NeedsSecrets: []string{}}
	file, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Import servers", Filters: []runtime.FileFilter{{DisplayName: "JSON (*.json)", Pattern: "*.json"}},
	})
	if err != nil || file == "" {
		return res, err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return res, err
	}
	var in exportFile
	if err := json.Unmarshal(b, &in); err != nil || in.Format != exportFormat {
		return res, errors.New("this is not a Server Dashboard export file")
	}
	newIDs := map[string]string{} // id in the file -> new id
	for _, s := range in.Servers {
		srv, err := a.st.AddImported(store.Server{Name: s.Name, Group: s.Group, Host: s.Host, Port: s.Port, Username: s.Username,
			AuthType: s.AuthType, PrivateKeyPath: s.PrivateKeyPath, UseSudo: s.UseSudo})
		if err != nil {
			res.Skipped = append(res.Skipped, s.Name)
			continue
		}
		newIDs[s.ID] = srv.ID
		res.Added = append(res.Added, srv.Name)
		if srv.AuthType == "password" || srv.AuthType == "key" {
			res.NeedsSecrets = append(res.NeedsSecrets, srv.Name)
		}
	}
	for _, s := range in.Servers {
		if s.JumpID == "" || newIDs[s.ID] == "" {
			continue
		}
		jump := newIDs[s.JumpID]
		if jump == "" {
			// The jump host was already in the list: link to it by address.
			for _, x := range in.Servers {
				if x.ID == s.JumpID {
					for _, have := range a.st.List() {
						if strings.EqualFold(have.Host, x.Host) && have.Port == x.Port && have.Username == x.Username {
							jump = have.ID
						}
					}
				}
			}
		}
		if jump != "" {
			_ = a.st.SetJump(newIDs[s.ID], jump)
		}
	}
	have := a.st.Snippets()
	for _, sn := range in.Snippets {
		dup := false
		for _, h := range have {
			if h.Name == sn.Name && h.Command == sn.Command {
				dup = true
			}
		}
		if dup {
			continue
		}
		sn.ID = ""
		if sn.ServerID != "" {
			sn.ServerID = newIDs[sn.ServerID]
		}
		if _, err := a.st.SaveSnippet(sn); err == nil {
			res.Snippets++
		}
	}
	a.audit("", "import servers", fmt.Sprintf("%s (%d added)", file, len(res.Added)), nil)
	return res, nil
}
