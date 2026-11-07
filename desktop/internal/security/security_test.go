package security

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseUfwRule(t *testing.T) {
	cases := []struct {
		line string
		want FwRule
	}{
		{"ufw allow 22/tcp", FwRule{Action: "allow", Port: "22", Proto: "tcp"}},
		{"ufw limit 22/tcp comment 'ssh from anywhere'", FwRule{Action: "limit", Port: "22", Proto: "tcp", Comment: "ssh from anywhere"}},
		{"ufw allow from 10.0.0.0/8 to any port 5432 proto tcp", FwRule{Action: "allow", From: "10.0.0.0/8", Port: "5432", Proto: "tcp"}},
		{"ufw deny from 1.2.3.4", FwRule{Action: "deny", From: "1.2.3.4"}},
		{"ufw allow 'Nginx Full'", FwRule{Action: "allow", App: "Nginx Full"}},
		{"ufw allow in on eth0 to any port 80", FwRule{Action: "allow", Iface: "eth0", Port: "80"}},
		{"ufw route allow from 10.0.0.0/8", FwRule{Action: "allow", Route: true, From: "10.0.0.0/8"}},
		{"ufw allow 6000:6007/udp", FwRule{Action: "allow", Port: "6000:6007", Proto: "udp"}},
	}
	for _, c := range cases {
		got, ok := parseUfwRule(c.line)
		if !ok {
			t.Fatalf("%q: not parsed", c.line)
		}
		w := c.want
		if w.From == "" {
			w.From = "any"
		}
		w.To, w.Dir, w.Cmd = "any", "in", strings.TrimPrefix(c.line, "ufw ")
		if got != w {
			t.Errorf("%q:\n got %+v\nwant %+v", c.line, got, w)
		}
	}
	if _, ok := parseUfwRule("Added user rules (see 'ufw status' for running firewall):"); ok {
		t.Error("header parsed as a rule")
	}
}

func rules(lines ...string) []FwRule {
	var out []FwRule
	for _, l := range lines {
		r, _ := parseUfwRule(l)
		out = append(out, r)
	}
	return out
}

func TestVerdict(t *testing.T) {
	me := net.ParseIP("203.0.113.7")
	rs := rules("ufw deny from 198.51.100.9", "ufw allow 22/tcp", "ufw allow from 10.0.0.0/8 to any port 5432 proto tcp", "ufw allow 'Nginx Full'")
	cases := []struct {
		port int
		src  net.IP
		want string
	}{
		{22, me, Allowed},
		{22, net.ParseIP("198.51.100.9"), Blocked}, // denied first
		{22, nil, Allowed},
		{443, nil, Allowed}, // application profile
		{5432, nil, Some},   // only from 10/8
		{5432, net.ParseIP("10.1.2.3"), Allowed},
		{6379, nil, Blocked},
	}
	for _, c := range cases {
		if got := Verdict(rs, "deny", c.port, "tcp", c.src); got != c.want {
			t.Errorf("port %d from %v: got %s, want %s", c.port, c.src, got, c.want)
		}
	}
	if Verdict(nil, "allow", 6379, "tcp", nil) != Allowed {
		t.Error("default allow not applied")
	}
}

func overviewWith(active bool, rs ...string) Overview {
	return Overview{Me: Me{IP: "203.0.113.7", SSHPort: 22}, Firewall: Firewall{Readable: true, UfwInstalled: true, UfwActive: active, DefaultIn: "deny", Rules: rules(rs...)}}
}

func TestKeepsAccess(t *testing.T) {
	o := overviewWith(true, "ufw allow 22/tcp")
	if err := keepsAccess(o, nil); err == nil {
		t.Error("deleting the only SSH rule while ufw is on must be refused")
	}
	if err := keepsAccess(o, rules("ufw allow 22/tcp", "ufw deny from 198.51.100.9")); err != nil {
		t.Errorf("blocking another address: %v", err)
	}
	if err := keepsAccess(o, rules("ufw deny from 203.0.113.0/24", "ufw allow 22/tcp")); err == nil {
		t.Error("blocking this computer's network must be refused")
	}
	off := overviewWith(false)
	if err := keepsAccess(off, rules("ufw allow 80/tcp")); err != nil {
		t.Errorf("ufw off, no SSH rule yet: %v", err)
	}
}

func TestNewRuleArgs(t *testing.T) {
	a, _, err := NewRule{Action: "allow", Port: "5432", Proto: "tcp", From: "10.0.0.0/8", Comment: "app servers"}.args()
	if err != nil || strings.Join(a, " ") != "allow proto tcp from 10.0.0.0/8 to any port 5432 comment app servers" {
		t.Errorf("args: %v %v", a, err)
	}
	bad := []NewRule{
		{Action: "allow"}, // every port from anywhere
		{Action: "allow", Port: "80;reboot"},
		{Action: "allow", Port: "70000"},
		{Action: "allow", Port: "6000:6007"}, // range without protocol
		{Action: "drop", Port: "22"},
		{Action: "deny", From: "1.2.3.4; rm -rf /"},
		{Action: "allow", Port: "22", Comment: "it's"},
	}
	for _, n := range bad {
		if _, _, err := n.args(); err == nil {
			t.Errorf("%+v accepted", n)
		}
	}
}

func TestParseFirewall(t *testing.T) {
	s := map[string][]string{
		"UFW":         {"installed", "Status: active"},
		"UFW_ADDED":   {"Added user rules (see 'ufw status' for running firewall):", "ufw allow 22/tcp", "ufw allow 80,443/tcp"},
		"UFW_DEFAULT": {`DEFAULT_INPUT_POLICY="DROP"`},
		"IPT":         {"-P INPUT ACCEPT", "-A INPUT -j ufw-before-logging-input", "-A DOCKER-USER -j RETURN"},
		"NFT":         {"ip filter\tINPUT\taccept\t4", "inet filter\tinput\taccept\t0"},
	}
	fw := parseFirewall(s, true)
	if !fw.Readable || fw.Kind != "ufw" || !fw.UfwActive || fw.DefaultIn != "deny" || len(fw.Rules) != 2 || fw.DockerUser || len(fw.Other) != 0 {
		t.Errorf("ufw: %+v", fw)
	}
	s = map[string][]string{"IPT": {"-P INPUT ACCEPT", "-A DOCKER-USER -s 10.0.0.0/8 -j RETURN", "-A DOCKER-USER -j DROP"},
		"NFT": {"inet filter\tinput\tdrop\t3"}}
	fw = parseFirewall(s, true)
	if fw.Kind != "nftables" || !fw.DockerUser || fw.UfwInstalled {
		t.Errorf("nftables: %+v", fw)
	}
	if fw := parseFirewall(map[string][]string{"UFW": {"installed", "ERROR: You need to be root to run this script"}}, false); fw.Readable {
		t.Errorf("not root: %+v", fw)
	}
	if fw := parseFirewall(map[string][]string{"IPT": {"-P INPUT ACCEPT"}}, true); fw.Kind != "none" {
		t.Errorf("no firewall: %+v", fw)
	}
}

func TestExposures(t *testing.T) {
	docker := []string{
		"db\t0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp",
		"web\t0.0.0.0:80->80/tcp, :::80->80/tcp, 0.0.0.0:443->443/tcp",
		"cache\t127.0.0.1:6379->6379/tcp",
		"worker\t8080/tcp",
	}
	listen := []string{
		"LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*",
		"LISTEN 0 4096 [::]:22 [::]:*",
		"LISTEN 0 4096 0.0.0.0:5432 0.0.0.0:*", // docker-proxy for db
		"LISTEN 0 4096 127.0.0.1:6379 0.0.0.0:*",
		"LISTEN 0 128 0.0.0.0:3306 0.0.0.0:*",
		"LISTEN 0 128 127.0.0.53%lo:53 0.0.0.0:*",
	}
	fw := Firewall{Readable: true, Kind: "ufw", UfwActive: true, UfwInstalled: true, DefaultIn: "deny", Rules: rules("ufw allow 22/tcp", "ufw allow 80/tcp", "ufw allow 443/tcp")}
	ex := exposures(fw, docker, listen, []int{22}, true)
	byKey := map[string]Exposure{}
	for _, e := range ex {
		byKey[e.Container+":"+itoa(e.Port)] = e
	}
	db := byKey["db:5432"]
	if db.Risk != "critical" || db.Service != "PostgreSQL" || len(db.Addrs) != 2 || !strings.Contains(db.Firewall, "Docker") {
		t.Errorf("db published past ufw: %+v", db)
	}
	if w := byKey["web:80"]; w.Risk != "info" || w.Firewall != "Allowed by ufw" {
		t.Errorf("web: %+v", w)
	}
	if _, ok := byKey["cache:6379"]; ok {
		t.Error("loopback port listed")
	}
	if _, ok := byKey[":3306"]; ok {
		t.Error("MySQL on the host is blocked by ufw: not exposed")
	}
	if s := byKey[":22"]; s.Service != "SSH" || s.Risk != "info" {
		t.Errorf("ssh: %+v", s)
	}
	if _, ok := byKey[":5432"]; ok {
		t.Error("docker-proxy listener listed as a host program")
	}
	if ex[0].Container != "db" {
		t.Errorf("critical first, got %+v", ex[0])
	}

	// No firewall: the host's MySQL is open too.
	ex = exposures(Firewall{Readable: true, Kind: "none"}, nil, listen, []int{22}, true)
	found := false
	for _, e := range ex {
		if e.Port == 3306 && e.Risk == "critical" {
			found = true
		}
	}
	if !found {
		t.Errorf("no firewall, MySQL: %+v", ex)
	}
}

var itoa = strconv.Itoa

func TestParseOverview(t *testing.T) {
	out := "@@ME\t1\tdeploy\t203.0.113.7 51234 192.0.2.10 2222\t12\n" +
		"@@SESSIONS\n@@LOGIND\n---\nId=12\nName=deploy\nTTY=\nRemoteHost=203.0.113.7\nRemote=yes\nService=sshd\nLeader=900\nClass=user\nState=active\nElapsed=120\n" +
		"---\nId=c3\nName=root\nTTY=pts/1\nRemoteHost=198.51.100.20\nRemote=yes\nService=sshd\nLeader=901\nClass=user\nState=active\nElapsed=30\n" +
		"---\nId=4\nName=deploy\nTTY=\nRemoteHost=\nRemote=no\nService=systemd-user\nLeader=902\nClass=manager\nState=active\n" +
		"@@SSHD\nport 2222\npasswordauthentication no\npermitrootlogin prohibit-password\npubkeyauthentication yes\nmaxauthtries 6\n" +
		"@@UFW\ninstalled\nStatus: inactive\n@@UFW_ADDED\nAdded user rules (see 'ufw status' for running firewall):\n(None)\n@@UFW_DEFAULT\nDEFAULT_INPUT_POLICY=\"DROP\"\n" +
		"@@IPT\n-P INPUT ACCEPT\n@@NFT\n@@DOCKER\n@@DOCKER_OK\n@@LISTEN\nLISTEN 0 128 0.0.0.0:2222 0.0.0.0:*\n"
	now := time.Unix(1_800_000_000, 0)
	o := parseOverview(out, now)
	if o.Me.User != "deploy" || !o.Me.Root || o.Me.IP != "203.0.113.7" || o.Me.SSHPort != 2222 || o.Me.SessionID != "12" {
		t.Errorf("me: %+v", o.Me)
	}
	if len(o.Sessions) != 2 || o.Sessions[0].ID != "c3" || !o.Sessions[1].Mine || o.Sessions[1].Since != now.Add(-120*time.Second).UnixMilli() {
		t.Errorf("sessions: %+v", o.Sessions)
	}
	if !o.SSH.Known || o.SSH.PasswordAuth || o.SSH.PermitRootLogin != "prohibit-password" || o.SSH.Ports[0] != 2222 {
		t.Errorf("sshd: %+v", o.SSH)
	}
	if o.Firewall.Kind != "none" || o.Firewall.UfwActive || !o.Docker {
		t.Errorf("firewall: %+v", o.Firewall)
	}
	if len(o.Exposed) != 1 || o.Exposed[0].Service != "SSH" {
		t.Errorf("exposed: %+v", o.Exposed)
	}
}

func TestParseLogins(t *testing.T) {
	out := "@@ME\t0\troot\t203.0.113.7 51234 192.0.2.10 22\t5\n@@DATA\n" +
		"T\t5\t2\t2\n" +
		"A\t1799990000\tpublickey\troot\t203.0.113.7\tSHA256:abc\tED25519\n" +
		"A\t1799995000\tpassword\tbob\t198.51.100.20\t\t\n" +
		"M\t1799990000\tpublickey\troot\t203.0.113.7\tSHA256:abc\tED25519\n" +
		"F\t45.1.1.1\t4\t1799900000\t1799999000\t3\tadmin,root,\n" +
		"F\t45.2.2.2\t1\t1799999500\t1799999500\t1\troot\n"
	lg := parseLogins(out, 7)
	if lg.Failed != 5 || lg.Failed24h != 2 || lg.Accepted != 2 || lg.AppKey != "SHA256:abc" {
		t.Errorf("totals: %+v", lg)
	}
	if len(lg.Recent) != 2 || lg.Recent[0].User != "bob" || !lg.Recent[1].Mine || lg.Recent[0].Mine {
		t.Errorf("recent: %+v", lg.Recent)
	}
	if len(lg.Sources) != 2 || lg.Sources[0].IP != "45.1.1.1" || len(lg.Sources[0].Users) != 3 || lg.Sources[0].Last != 1799999000000 {
		t.Errorf("sources: %+v", lg.Sources)
	}
}

const edKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEzFx0AehbIP3ilIDiDGPqp0cJ4ZbKtHDdSvP7Z4sBqv"

func TestParseKeys(t *testing.T) {
	file := "# admins\n" + edKey + " alice@laptop\nfrom=\"10.0.0.0/8\",no-pty " + edKey + " backup\nnot a key\n"
	out := "@@ME\t1\tdeploy\t\t\n@@AKF\t.ssh/authorized_keys .ssh/authorized_keys2\n" +
		"@@USER\troot\t0\t/root\t/bin/bash\n@@USER\tdeploy\t1000\t/home/deploy\t/bin/bash\n@@USER\twww-data\t33\t/var/www\t/usr/sbin/nologin\n" +
		"@@FILE\tdeploy\t/home/deploy/.ssh/authorized_keys\t" + itoa(len(file)) + "\t1800000000\n" + file +
		"@@FILE\troot\t/root/.ssh/authorized_keys\t-1\t0\n"
	k := parseKeys(out)
	if !k.Root || len(k.Users) != 3 || k.Users[2].CanLogin || len(k.Patterns) != 2 {
		t.Errorf("keys: %+v", k)
	}
	if len(k.Files) != 2 {
		t.Fatalf("files: %+v", k.Files)
	}
	f := k.Files[0]
	if len(f.Keys) != 3 || f.Keys[0].Comment != "alice@laptop" || f.Keys[0].Type != "ssh-ed25519" || f.Keys[0].Line != 2 {
		t.Errorf("keys of deploy: %+v", f.Keys)
	}
	if len(f.Keys[1].Options) != 2 || f.Keys[1].Fingerprint != f.Keys[0].Fingerprint || f.Keys[2].Invalid == "" {
		t.Errorf("options / invalid: %+v", f.Keys)
	}
	if k.Files[1].Readable {
		t.Error("unreadable root file")
	}
	if got := expand("%h/.ssh/authorized_keys", k.Users[1]); got != "/home/deploy/.ssh/authorized_keys" {
		t.Error(got)
	}
	if got := expand("/etc/ssh/keys/%u", k.Users[1]); got != "/etc/ssh/keys/deploy" {
		t.Error(got)
	}
}
