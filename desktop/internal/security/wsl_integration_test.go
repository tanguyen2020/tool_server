package security

// Runs the real scripts against WSL (Ubuntu with systemd, ufw and iptables):
//
//	SECURITY_WSL=1 go test ./internal/security -run WSL -v
//
// root plays a server where the app connects as root; the distribution's default user one where sudo asks
// for a password. The test adds a user (sdtest), journal lines and an inactive ufw rule, and removes them.

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type wslHost struct {
	user string
	dir  string
}

type wslExit struct{ code int }

func (e *wslExit) Error() string { return "exit " + itoa(e.code) }
func (e *wslExit) ExitCode() int { return e.code }

func (h wslHost) Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	f, err := os.CreateTemp(h.dir, "cmd-*.sh")
	if err != nil {
		return err
	}
	_, _ = f.WriteString(cmd)
	f.Close()
	args := []string{"-d", "Ubuntu"}
	if h.user != "" {
		args = append(args, "-u", h.user)
	}
	p := filepath.ToSlash(f.Name())
	p = "/mnt/" + strings.ToLower(p[:1]) + p[2:]
	c := exec.CommandContext(ctx, "wsl.exe", append(args, "--", "sh", p)...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return &wslExit{ee.ExitCode()}
	}
	return err
}

func TestWSL(t *testing.T) {
	if os.Getenv("SECURITY_WSL") == "" {
		t.Skip("set SECURITY_WSL=1 to run the scripts in WSL")
	}
	ctx := context.Background()
	dir := t.TempDir()
	root := wslHost{user: "root", dir: dir}
	plain := wslHost{dir: dir}
	sh := func(cmd string) string {
		var o, e strings.Builder
		if err := root.Run(ctx, cmd, nil, &o, &e); err != nil {
			t.Fatalf("%s: %v %s", cmd, err, e.String())
		}
		return o.String()
	}

	t.Run("overview", func(t *testing.T) {
		o, err := ReadOverview(ctx, root, "docker")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("me %+v by %s sessions %+v", o.Me, o.SessionsBy, o.Sessions)
		t.Logf("firewall %+v", o.Firewall)
		t.Logf("exposed %+v", o.Exposed)
		if !o.Me.Root || o.SessionsBy != "logind" || !o.Firewall.Readable || !o.Firewall.UfwInstalled {
			t.Errorf("as root: %+v", o)
		}
		u, err := ReadOverview(ctx, plain, "docker")
		if err != nil {
			t.Fatal(err)
		}
		if u.Me.Root || u.Firewall.Readable {
			t.Errorf("as a user: me %+v firewall %+v", u.Me, u.Firewall)
		}
	})

	t.Run("logins", func(t *testing.T) {
		// Three bot connections from one address (one counts once per process), one from another, one login.
		sh(`logger -t sshd "Invalid user admin from 192.0.2.66 port 40001"
logger -t sshd "Failed password for root from 192.0.2.66 port 40002 ssh2"
logger -t sshd "Connection closed by authenticating user root 192.0.2.66 port 40003 [preauth]"
logger -t sshd "Disconnected from invalid user oracle 192.0.2.77 port 40004 [preauth]"
logger -t sshd "Accepted publickey for sdtest from 192.0.2.10 port 40005 ssh2: ED25519 SHA256:wsltestkey"
sleep 1`)
		lg, err := ReadLogins(ctx, root, 1)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%+v", lg)
		var a66 *FailSource
		for i := range lg.Sources {
			if lg.Sources[i].IP == "192.0.2.66" {
				a66 = &lg.Sources[i]
			}
		}
		if a66 == nil || a66.Attempts < 3 || !strings.Contains(strings.Join(a66.Users, ","), "admin") {
			t.Errorf("192.0.2.66: %+v", a66)
		}
		found := false
		for _, r := range lg.Recent {
			if r.User == "sdtest" && r.Fingerprint == "SHA256:wsltestkey" && r.KeyType == "ED25519" {
				found = true
			}
		}
		if !found || lg.Failed24h < 4 {
			t.Errorf("recent / totals: %+v", lg)
		}
	})

	t.Run("keys", func(t *testing.T) {
		sh(`id sdtest >/dev/null 2>&1 || useradd -m -s /bin/bash sdtest`)
		defer sh(`userdel -r sdtest 2>/dev/null; true`)
		if _, err := AddKey(ctx, root, "sdtest", edKey+" first@test"); err != nil {
			t.Fatal(err)
		}
		if got := sh(`stat -c '%U %a' /home/sdtest/.ssh /home/sdtest/.ssh/authorized_keys`); got != "sdtest 700\nsdtest 600\n" {
			t.Errorf("owner and mode: %q", got)
		}
		if _, err := AddKey(ctx, root, "sdtest", edKey+" again"); err == nil || !strings.Contains(err.Error(), "already") {
			t.Errorf("duplicate: %v", err)
		}
		sh(`printf '%s\n' "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl other" >> /home/sdtest/.ssh/authorized_keys; chmod 640 /home/sdtest/.ssh/authorized_keys`)
		k, err := ReadKeys(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		var f *KeyFile
		for i := range k.Files {
			if k.Files[i].User == "sdtest" {
				f = &k.Files[i]
			}
		}
		if f == nil || len(f.Keys) != 2 {
			t.Fatalf("sdtest keys: %+v", f)
		}
		if err := RemoveKey(ctx, root, "sdtest", f.Path, f.Keys[0].Fingerprint); err != nil {
			t.Fatal(err)
		}
		if got := sh(`cat /home/sdtest/.ssh/authorized_keys; stat -c '%U %a' /home/sdtest/.ssh/authorized_keys`); !strings.HasSuffix(got, " other\nsdtest 640\n") || strings.Contains(got, "first@test") {
			t.Errorf("after remove: %q", got)
		}
		// A user that is not root cannot touch another user's keys.
		if err := RemoveKey(ctx, plain, "sdtest", f.Path, f.Keys[1].Fingerprint); err == nil {
			t.Error("removed another user's key without root")
		}
	})

	t.Run("new user", func(t *testing.T) {
		defer sh(`userdel -r sdnew 2>/dev/null; rm -f /etc/sudoers.d/90-serverdash-sdnew; true`)
		if err := CreateUser(ctx, root, "Bad Name", NewUser{Create: true}); err == nil {
			t.Error("invalid name accepted")
		}
		if err := CreateUser(ctx, plain, "sdnew", NewUser{Create: true}); err == nil {
			t.Error("created a user without root")
		}
		k, _ := ReadKeys(ctx, root)
		if err := CreateUser(ctx, root, "sdnew", NewUser{Create: true, Docker: k.Docker, Sudo: true}); err != nil {
			t.Fatal(err)
		}
		if err := CreateUser(ctx, root, "sdnew", NewUser{Create: true}); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("second create: %v", err)
		}
		if _, err := AddKey(ctx, root, "sdnew", edKey+" new@test"); err != nil {
			t.Fatal(err)
		}
		got := sh(`getent shadow sdnew | cut -d: -f2; getent passwd sdnew | cut -d: -f7; id -nG sdnew; stat -c '%a' /etc/sudoers.d/90-serverdash-sdnew; visudo -c >/dev/null && echo sudoers-ok; sudo -l -U sdnew | tail -1; stat -c '%U %a' /home/sdnew/.ssh/authorized_keys`)
		t.Logf("%s", got)
		for _, want := range []string{"*\n/bin/bash\n", "440\n", "sudoers-ok", "NOPASSWD: ALL", "sdnew 600"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in %q", want, got)
			}
		}
		if k.Docker && !strings.Contains(got, "docker") {
			t.Errorf("not in the docker group: %q", got)
		}
	})

	t.Run("ufw rules while off", func(t *testing.T) {
		o, err := ReadOverview(ctx, root, "docker")
		if err != nil {
			t.Fatal(err)
		}
		if o.Firewall.UfwActive {
			t.Skip("ufw is on in this WSL: not touching it")
		}
		if err := AddRule(ctx, root, o, NewRule{Action: "allow", Port: "5999", Proto: "tcp", From: "10.9.9.0/24", Comment: "sdtest"}); err != nil {
			t.Fatal(err)
		}
		o, _ = ReadOverview(ctx, root, "docker")
		if err := AddRule(ctx, root, o, NewRule{Action: "deny", From: "192.0.2.66", Comment: "sdtest block"}); err != nil {
			t.Fatal(err)
		}
		o, _ = ReadOverview(ctx, root, "docker")
		var cmds []string
		for _, r := range o.Firewall.Rules {
			if r.Comment == "sdtest" || r.Comment == "sdtest block" {
				cmds = append(cmds, r.Cmd)
			}
		}
		t.Logf("rules %+v", o.Firewall.Rules)
		if len(cmds) != 2 || o.Firewall.Rules[0].Comment != "sdtest block" {
			t.Fatalf("rules after add: %+v", o.Firewall.Rules)
		}
		if v := Verdict(o.Firewall.Rules, o.Firewall.DefaultIn, 5999, "tcp", nil); v != Some {
			t.Errorf("verdict 5999: %s", v)
		}
		for _, c := range cmds {
			if err := DeleteRule(ctx, root, o, c); err != nil {
				t.Error(err)
			}
		}
		o, _ = ReadOverview(ctx, root, "docker")
		for _, r := range o.Firewall.Rules {
			if strings.HasPrefix(r.Comment, "sdtest") {
				t.Errorf("left behind: %+v", r)
			}
		}
	})
}
