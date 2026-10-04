// Package sshpool keeps one long-lived SSH connection per server; every command runs on
// a session of that connection (OpenSSH allows 10 sessions per connection by default).
package sshpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"serverdash/internal/store"
)

// DockerBin returns the docker command, with sudo when the user is not in the docker group.
func DockerBin(s store.Server) string {
	if s.UseSudo {
		return "sudo -n docker"
	}
	return "docker"
}

type Result struct {
	Stdout string
	Stderr string
	Code   int
}

type Conn struct {
	mu       sync.Mutex
	srv      store.Server
	client   *ssh.Client
	onHost   func(fingerprint string)
	via      Via // jump host, nil for a direct connection
	lastErr  error
	lastFail time.Time
	closed   bool

	sftp   *sftp.Client
	sftpOf *ssh.Client // the connection the SFTP client runs on
}

// Via returns the SSH connection of the jump host (bastion) a server is reached through.
type Via func() (*ssh.Client, error)

// retryAfter: after a failed connection, wait before retrying so the server is not hammered.
const retryAfter = 10 * time.Second

func authMethods(c store.Credentials) ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	switch c.AuthType {
	case "password":
		pw := c.PlainPassword
		return []ssh.AuthMethod{
			ssh.Password(pw),
			// Many Debian servers only allow password login via keyboard-interactive.
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range ans {
					ans[i] = pw
				}
				return ans, nil
			}),
		}, noop, nil
	case "key", "keyPath":
		var signer ssh.Signer
		var err error
		if c.PlainPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(c.PlainPrivateKey, []byte(c.PlainPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(c.PlainPrivateKey)
		}
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, noop, errors.New("the private key is protected, enter its passphrase")
		}
		if err != nil {
			return nil, noop, fmt.Errorf("invalid private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, noop, nil
	case "agent":
		ag, closeFn, err := dialAgent()
		if err != nil {
			return nil, noop, fmt.Errorf("cannot connect to SSH agent: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}, closeFn, nil
	}
	return nil, noop, errors.New("invalid authentication type")
}

func dial(srv store.Server, onHost func(string), via Via) (*ssh.Client, error) {
	creds, err := store.Decrypt(srv)
	if err != nil {
		return nil, err
	}
	auth, cleanup, err := authMethods(creds)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var hostErr error
	cfg := &ssh.ClientConfig{
		User:    srv.Username,
		Auth:    auth,
		Timeout: 10 * time.Second,
		// Trust-on-first-use: remember the fingerprint the first time, require a match afterwards (prevents MITM).
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			if srv.HostKey == "" {
				onHost(fp)
				return nil
			}
			if srv.HostKey != fp {
				hostErr = fmt.Errorf("host key has changed (%s). If you just reinstalled the server, click \"Reset host key\"", fp)
				return hostErr
			}
			return nil
		},
	}
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))
	var client *ssh.Client
	if via == nil {
		client, err = ssh.Dial("tcp", addr, cfg)
	} else {
		client, err = dialVia(via, addr, cfg)
	}
	if hostErr != nil {
		return nil, hostErr
	}
	if err != nil {
		return nil, friendly(err)
	}
	return client, nil
}

// dialVia opens the SSH connection through a TCP channel of the jump host's connection (like ssh -J).
func dialVia(via Via, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	jump, err := via()
	if err != nil {
		return nil, fmt.Errorf("jump host: %w", err)
	}
	nc, err := jump.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("jump host cannot reach %s: %w", addr, err)
	}
	// The handshake has no deadline on a channel: close it if the server does not answer in time.
	timer := time.AfterFunc(cfg.Timeout, func() { _ = nc.Close() })
	c, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if !timer.Stop() && err != nil {
		return nil, errors.New("connection timed out (10s) through the jump host")
	}
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func friendly(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"):
		return errors.New("authentication failed: wrong user, password or key")
	case strings.Contains(msg, "i/o timeout"):
		return errors.New("connection timed out (10s): check IP, port and firewall")
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused"):
		return errors.New("connection refused: SSH is not running or wrong port")
	case strings.Contains(msg, "no such host"):
		return errors.New("cannot resolve host name")
	}
	return err
}

func (c *Conn) get() (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("connection closed")
	}
	if c.client != nil {
		return c.client, nil
	}
	if c.lastErr != nil && time.Since(c.lastFail) < retryAfter {
		return nil, c.lastErr
	}
	client, err := dial(c.srv, func(fp string) {
		c.srv.HostKey = fp
		c.onHost(fp)
	}, c.via)
	if err != nil {
		c.lastErr, c.lastFail = err, time.Now()
		return nil, err
	}
	c.client, c.lastErr = client, nil
	go c.keepalive(client)
	return client, nil
}

// keepalive detects dead connections (network drop, server reboot) so the next call reconnects.
func (c *Conn) keepalive(client *ssh.Client) {
	done := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(done)
	}()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-done:
			c.drop(client)
			return
		case <-t.C:
			errc := make(chan error, 1)
			go func() {
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				errc <- err
			}()
			select {
			case err := <-errc:
				if err != nil {
					fails++
				} else {
					fails = 0
				}
			case <-time.After(10 * time.Second):
				fails++
			}
			if fails >= 3 {
				_ = client.Close()
			}
		}
	}
}

func (c *Conn) drop(client *ssh.Client) {
	c.mu.Lock()
	if c.client == client {
		c.client = nil
	}
	c.mu.Unlock()
}

func (c *Conn) Close() {
	c.mu.Lock()
	c.closed = true
	if c.sftp != nil {
		_ = c.sftp.Close()
		c.sftp = nil
	}
	if c.client != nil {
		_ = c.client.Close()
		c.client = nil
	}
	c.mu.Unlock()
}

// Client returns the live SSH connection (connecting when needed). Used to reach other servers through it.
func (c *Conn) Client() (*ssh.Client, error) { return c.get() }

// Dial opens a TCP connection from the server's side (port forwarding).
func (c *Conn) Dial(addr string) (net.Conn, error) {
	client, err := c.get()
	if err != nil {
		return nil, err
	}
	return client.Dial("tcp", addr)
}

// SFTP returns an SFTP client on the existing connection (no new login); recreated after a reconnect.
func (c *Conn) SFTP() (*sftp.Client, error) {
	client, err := c.get()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sftp != nil && c.sftpOf == client {
		return c.sftp, nil
	}
	if c.sftp != nil {
		_ = c.sftp.Close()
	}
	s, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("SFTP is not available on this server: %w", err)
	}
	c.sftp, c.sftpOf = s, client
	return s, nil
}

// Exec runs a command and waits for its result, up to timeout.
func (c *Conn) Exec(ctx context.Context, cmd string, timeout time.Duration) (Result, error) {
	client, err := c.get()
	if err != nil {
		return Result{}, err
	}
	sess, err := client.NewSession()
	if err != nil {
		// Failing to open a session usually means the connection is broken -> drop it and reconnect next time.
		if !strings.Contains(err.Error(), "administratively prohibited") {
			_ = client.Close()
			c.drop(client)
		}
		return Result{}, fmt.Errorf("cannot open SSH session: %w", err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-timer.C:
		_ = sess.Close()
		return Result{}, fmt.Errorf("command timed out after %v", timeout)
	case <-ctx.Done():
		_ = sess.Close()
		return Result{}, ctx.Err()
	}
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		res.Code = exitErr.ExitStatus()
		return res, nil
	}
	return res, err
}

// Stream runs a long-running command (docker logs -f) and returns a stdout+stderr reader.
type Stream struct {
	sess *ssh.Session
	io.Reader
}

func (s *Stream) Stop() {
	_ = s.sess.Signal(ssh.SIGTERM)
	_ = s.sess.Close()
}

func (s *Stream) Wait() error { return s.sess.Wait() }

func (c *Conn) Stream(cmd string) (*Stream, error) {
	client, err := c.get()
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("cannot open SSH session: %w", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		return nil, err
	}
	return &Stream{sess: sess, Reader: out}, nil
}

type Pool struct {
	mu    sync.Mutex
	st    *store.Store
	conns map[string]*Conn
}

func New(st *store.Store) *Pool {
	p := &Pool{st: st, conns: map[string]*Conn{}}
	// Server config changed or removed -> drop the old connection.
	st.OnChange(p.Drop)
	return p
}

func (p *Pool) Get(id string) (*Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[id]; ok {
		return c, nil
	}
	srv, ok := p.st.Get(id)
	if !ok {
		return nil, errors.New("server not found")
	}
	c := &Conn{srv: srv, onHost: func(fp string) { p.st.SetHostKey(id, fp) }, via: p.via(srv.JumpID)}
	p.conns[id] = c
	return c, nil
}

// via returns how to reach the jump host, or nil for a direct connection.
func (p *Pool) via(jumpID string) Via {
	if jumpID == "" {
		return nil
	}
	return func() (*ssh.Client, error) {
		jc, err := p.Get(jumpID)
		if err != nil {
			return nil, err
		}
		return jc.get()
	}
}

// Via is exported for testing an unsaved server that goes through a jump host.
func (p *Pool) Via(jumpID string) Via { return p.via(jumpID) }

// Drop closes a server's connection and the ones that go through it (it may be their jump host).
func (p *Pool) Drop(id string) {
	p.mu.Lock()
	c := p.conns[id]
	delete(p.conns, id)
	var through []string
	for k, x := range p.conns {
		if x.srv.JumpID == id {
			through = append(through, k)
		}
	}
	p.mu.Unlock()
	if c != nil {
		c.Close()
	}
	for _, k := range through {
		p.Drop(k)
	}
}

func (p *Pool) CloseAll() {
	p.mu.Lock()
	conns := p.conns
	p.conns = map[string]*Conn{}
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

type TestResult struct {
	Hostname string `json:"hostname"`
	Docker   string `json:"docker"`
	HostKey  string `json:"hostKey"`
}

// Test tries a connection with an unsaved config (the "Add server" form).
func Test(ctx context.Context, srv store.Server, via Via) (TestResult, error) {
	c := &Conn{srv: srv, onHost: func(string) {}, via: via}
	defer c.Close()
	res, err := c.Exec(ctx, "hostname; "+DockerBin(srv)+" version --format '{{.Server.Version}}' 2>&1 | head -1", 15*time.Second)
	if err != nil {
		return TestResult{}, err
	}
	lines := strings.SplitN(strings.TrimSpace(res.Stdout), "\n", 2)
	out := TestResult{Hostname: lines[0], HostKey: c.srv.HostKey}
	if len(lines) > 1 {
		out.Docker = strings.TrimSpace(lines[1])
	}
	return out, nil
}

// Term is an interactive shell with a pseudo-terminal, opened on the server's existing connection.
type Term struct {
	sess  *ssh.Session
	stdin io.WriteCloser
	out   io.Reader
}

func (t *Term) Read(p []byte) (int, error)  { return t.out.Read(p) }
func (t *Term) Write(p []byte) (int, error) { return t.stdin.Write(p) }
func (t *Term) Resize(cols, rows int) error { return t.sess.WindowChange(rows, cols) }
func (t *Term) Wait() error                 { return t.sess.Wait() }
func (t *Term) Close()                      { _ = t.sess.Close() }

// Terminal opens a new session with a PTY: the login shell when command is empty, otherwise command.
// No new SSH connection or login is needed — it reuses the one the dashboard already holds.
func (c *Conn) Terminal(cols, rows int, command string) (*Term, error) {
	client, err := c.get()
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("cannot open SSH session: %w", err)
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("cannot allocate a terminal: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if command == "" {
		err = sess.Shell()
	} else {
		err = sess.Start(command)
	}
	if err != nil {
		sess.Close()
		return nil, err
	}
	return &Term{sess: sess, stdin: stdin, out: out}, nil
}
