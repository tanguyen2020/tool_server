package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Firewall is what filters incoming connections: ufw (managed here), or rules of another tool (shown only).
type Firewall struct {
	Readable     bool     `json:"readable"` // the state could be read (needs root)
	Kind         string   `json:"kind"`     // ufw, nftables, iptables, none ("" when not readable)
	UfwInstalled bool     `json:"ufwInstalled"`
	UfwActive    bool     `json:"ufwActive"`
	DefaultIn    string   `json:"defaultIn"`  // ufw's default for incoming: deny, reject, allow
	Rules        []FwRule `json:"rules"`      // ufw rules in the order ufw applies them
	DockerUser   bool     `json:"dockerUser"` // the DOCKER-USER chain filters traffic to containers
	Other        []string `json:"other"`      // filtering found outside ufw, in plain words
}

// FwRule is one ufw rule, as `ufw show added` prints it.
type FwRule struct {
	Cmd     string `json:"cmd"`    // the rule without "ufw " (allow 22/tcp), used to delete it
	Action  string `json:"action"` // allow, deny, reject, limit
	Dir     string `json:"dir"`    // in, out
	Route   bool   `json:"route"`  // forwarded traffic, not traffic to the server
	Iface   string `json:"iface"`  // on eth0
	Proto   string `json:"proto"`  // tcp, udp, "" = any
	From    string `json:"from"`   // any, or an address / network
	To      string `json:"to"`     // any, or an address of the server
	Port    string `json:"port"`   // 22, 80,443, 6000:6007, "" = every port
	App     string `json:"app"`    // an application profile (OpenSSH, Nginx Full…)
	Comment string `json:"comment"`
}

// Ports of the application profiles that ship with common Debian packages.
var appPorts = map[string]string{
	"openssh": "22", "nginx http": "80", "nginx https": "443", "nginx full": "80,443",
	"apache": "80", "apache secure": "443", "apache full": "80,443", "www": "80", "www secure": "443", "www full": "80,443",
	"postfix": "25", "postfix smtps": "465", "postfix submission": "587", "dovecot imap": "143", "dovecot secure imap": "993",
}

func parseFirewall(s map[string][]string, root bool) Firewall {
	fw := Firewall{Rules: []FwRule{}, Other: []string{}}
	ufw, hasUfw := s["UFW"]
	fw.UfwInstalled = hasUfw && len(ufw) > 0 && ufw[0] == "installed"
	if fw.UfwInstalled {
		for _, l := range ufw[1:] {
			if strings.HasPrefix(l, "Status:") {
				fw.Readable = true
				fw.UfwActive = strings.TrimSpace(strings.TrimPrefix(l, "Status:")) == "active"
			}
		}
		for _, l := range s["UFW_ADDED"] {
			if r, ok := parseUfwRule(l); ok {
				fw.Rules = append(fw.Rules, r)
			}
		}
		fw.DefaultIn = "deny"
		for _, l := range s["UFW_DEFAULT"] {
			if v, ok := strings.CutPrefix(l, "DEFAULT_INPUT_POLICY="); ok {
				switch strings.ToUpper(strings.Trim(v, `"' `)) {
				case "ACCEPT":
					fw.DefaultIn = "allow"
				case "REJECT":
					fw.DefaultIn = "reject"
				}
			}
		}
	} else if root {
		fw.Readable = true
	}
	if !fw.Readable {
		return fw
	}

	// iptables: the INPUT chain (not ufw's) and DOCKER-USER.
	customInput := 0
	for _, l := range s["IPT"] {
		f := strings.Fields(l)
		switch {
		case len(f) >= 3 && f[0] == "-P" && f[1] == "INPUT" && f[2] != "ACCEPT":
			fw.Other = append(fw.Other, "iptables drops incoming traffic by default (INPUT policy "+f[2]+")")
		case len(f) >= 2 && f[0] == "-A" && f[1] == "INPUT" && !strings.Contains(l, "ufw-"):
			customInput++
		case len(f) >= 2 && f[0] == "-A" && f[1] == "DOCKER-USER" && strings.Join(f[2:], " ") != "-j RETURN":
			fw.DockerUser = true
		}
	}
	if customInput > 0 {
		fw.Other = append(fw.Other, fmt.Sprintf("%d iptables rule%s in the INPUT chain", customInput, plural(customInput)))
	}
	// nftables: input chains of tables iptables-nft did not create (those are "ip filter" / "ip6 filter").
	for _, l := range s["NFT"] {
		f := strings.Split(l, "\t")
		if len(f) < 4 || f[0] == "ip filter" || f[0] == "ip6 filter" {
			continue
		}
		n, _ := strconv.Atoi(f[3])
		if f[2] == "drop" || n > 0 {
			fw.Other = append(fw.Other, fmt.Sprintf("nftables table %s, chain %s: %d rule%s, policy %s", f[0], f[1], n, plural(n), orStr(f[2], "accept")))
		}
	}
	switch {
	case fw.UfwActive:
		fw.Kind = "ufw"
	case len(fw.Other) > 0 && strings.HasPrefix(fw.Other[len(fw.Other)-1], "nftables"):
		fw.Kind = "nftables"
	case len(fw.Other) > 0:
		fw.Kind = "iptables"
	default:
		fw.Kind = "none"
	}
	return fw
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// tokens splits a ufw rule line; 'quoted words' (comments, profile names) stay one token.
func tokens(l string) []string {
	var out []string
	var b strings.Builder
	in, has := false, false
	for _, r := range l {
		switch {
		case r == '\'':
			in = !in
			has = true
		case r == ' ' && !in:
			if has {
				out = append(out, b.String())
				b.Reset()
				has = false
			}
		default:
			b.WriteRune(r)
			has = true
		}
	}
	if has {
		out = append(out, b.String())
	}
	return out
}

var simplePortRe = regexp.MustCompile(`^([0-9]+([:,][0-9]+)*)(/(tcp|udp))?$`)

func parseUfwRule(l string) (FwRule, bool) {
	t := tokens(strings.TrimSpace(l))
	if len(t) < 3 || t[0] != "ufw" {
		return FwRule{}, false
	}
	r := FwRule{Cmd: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "ufw ")), From: "any", To: "any", Dir: "in"}
	i := 1
	if t[i] == "route" {
		r.Route = true
		i++
	}
	switch t[i] {
	case "allow", "deny", "reject", "limit":
		r.Action = t[i]
	default:
		return FwRule{}, false
	}
	i++
	for ; i < len(t); i++ {
		next := func() string {
			if i+1 < len(t) {
				i++
				return t[i]
			}
			return ""
		}
		switch t[i] {
		case "in", "out":
			r.Dir = t[i]
		case "on":
			r.Iface = next()
		case "log", "log-all":
		case "proto":
			r.Proto = next()
		case "from":
			r.From = next()
		case "to":
			r.To = next()
		case "port":
			r.Port = next()
		case "app":
			r.App = next()
		case "comment":
			r.Comment = next()
		default:
			if m := simplePortRe.FindStringSubmatch(t[i]); m != nil {
				r.Port, r.Proto = m[1], m[4]
			} else {
				r.App = t[i]
			}
		}
	}
	return r, true
}

// portsOf lists the port ranges a rule covers; nil means every port.
func (r FwRule) portsOf() ([][2]int, bool) {
	spec := r.Port
	if r.App != "" {
		p, ok := appPorts[strings.ToLower(r.App)]
		if !ok {
			return nil, false // unknown profile: cannot tell
		}
		spec = p
	}
	if spec == "" {
		return nil, true
	}
	var out [][2]int
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a, err1 := strconv.Atoi(lo)
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil {
			return nil, false
		}
		out = append(out, [2]int{a, b})
	}
	return out, true
}

func (r FwRule) covers(port int, proto string) bool {
	if r.Proto != "" && proto != "" && r.Proto != proto {
		return false
	}
	ranges, ok := r.portsOf()
	if !ok {
		return false
	}
	if ranges == nil {
		return true
	}
	for _, x := range ranges {
		if port >= x[0] && port <= x[1] {
			return true
		}
	}
	return false
}

func isAny(addr string) bool {
	return addr == "" || addr == "any" || addr == "0.0.0.0/0" || addr == "::/0"
}

// fromMatches: src nil means "anyone on the network"; a rule for some addresses only does not match it.
func fromMatches(from string, src net.IP) bool {
	if isAny(from) {
		return true
	}
	if src == nil {
		return false
	}
	if _, n, err := net.ParseCIDR(from); err == nil {
		return n.Contains(src)
	}
	ip := net.ParseIP(from)
	return ip != nil && ip.Equal(src)
}

// Verdicts of Verdict.
const (
	Allowed = "allowed" // ufw lets it in
	Some    = "some"    // only from some addresses
	Blocked = "blocked"
)

// Verdict says whether ufw lets a connection to port/proto in, from src (nil: from anyone).
// ufw applies the first rule that matches, then its default policy.
func Verdict(rules []FwRule, defaultIn string, port int, proto string, src net.IP) string {
	some := false
	for _, r := range rules {
		if r.Route || r.Dir == "out" || !r.covers(port, proto) {
			continue
		}
		if !fromMatches(r.From, src) {
			if src == nil && (r.Action == "allow" || r.Action == "limit") {
				some = true
			}
			continue
		}
		if r.Action == "allow" || r.Action == "limit" {
			return Allowed
		}
		if some {
			return Some
		}
		return Blocked
	}
	if defaultIn == "allow" {
		return Allowed
	}
	if some {
		return Some
	}
	return Blocked
}

// ---------------------------------------------------------------- changes

// NewRule is a rule added from the app.
type NewRule struct {
	Action  string `json:"action"` // allow, deny, reject, limit
	Port    string `json:"port"`   // "" = every port, 22, 80,443, 6000:6007
	Proto   string `json:"proto"`  // tcp, udp, any
	From    string `json:"from"`   // "" / any, or an address / network
	Comment string `json:"comment"`
}

var portSpecRe = regexp.MustCompile(`^[0-9]{1,5}(:[0-9]{1,5})?(,[0-9]{1,5}(:[0-9]{1,5})?){0,14}$`)

// args validates the rule and returns ufw's words for it, and the rule as ufw will list it.
func (n NewRule) args() ([]string, FwRule, error) {
	r := FwRule{Action: n.Action, Dir: "in", To: "any", From: "any", Port: strings.TrimSpace(n.Port), Comment: strings.TrimSpace(n.Comment)}
	switch n.Action {
	case "allow", "deny", "reject", "limit":
	default:
		return nil, r, errors.New("choose allow, deny, reject or limit")
	}
	switch n.Proto {
	case "tcp", "udp":
		r.Proto = n.Proto
	case "", "any":
	default:
		return nil, r, errors.New("the protocol must be tcp, udp or any")
	}
	if r.Port != "" {
		if !portSpecRe.MatchString(r.Port) {
			return nil, r, errors.New("enter a port (22), a list (80,443) or a range (6000:6007)")
		}
		ranges, _ := r.portsOf()
		for _, x := range ranges {
			if x[0] < 1 || x[1] > 65535 || x[0] > x[1] {
				return nil, r, errors.New("ports must be between 1 and 65535")
			}
		}
		if (strings.ContainsAny(r.Port, ":,")) && r.Proto == "" {
			return nil, r, errors.New("a list or range of ports needs a protocol (tcp or udp)")
		}
	}
	if n.Action == "limit" && r.Proto == "" {
		r.Proto = "tcp"
	}
	from := strings.TrimSpace(n.From)
	if !isAny(from) {
		if ip := net.ParseIP(from); ip != nil {
			r.From = ip.String()
		} else if _, nw, err := net.ParseCIDR(from); err == nil {
			r.From = nw.String()
		} else {
			return nil, r, errors.New("the source must be an IP address or a network such as 10.0.0.0/8")
		}
	}
	if r.Port == "" && r.From == "any" {
		return nil, r, errors.New("choose a port or a source address: a rule for every port from anywhere is too broad")
	}
	if len(r.Comment) > 100 || strings.ContainsAny(r.Comment, "'\"\\\n\r") {
		return nil, r, errors.New("the comment must be under 100 characters, without quotes")
	}
	a := []string{r.Action}
	if r.Proto != "" {
		a = append(a, "proto", r.Proto)
	}
	a = append(a, "from", r.From, "to", "any")
	if r.Port != "" {
		a = append(a, "port", r.Port)
	}
	if r.Comment != "" {
		a = append(a, "comment", r.Comment)
	}
	return a, r, nil
}

// ufwBody runs ufw with the arguments from $4 on, as root.
const ufwBody = `export LC_ALL=C PATH="$PATH:/usr/sbin:/sbin"
[ "$1" = 2 ] && { echo "You need to be root to run this script" >&2; exit 1; }
shift 3
ufw "$@"`

func runUfw(ctx context.Context, h Host, what string, args ...string) (string, error) {
	out, stderr, code, err := run(ctx, h, wrap(ufwBody, args...), nil, 60*time.Second)
	if err != nil {
		return "", err
	}
	_, out = parseMe(out)
	if code != 0 || strings.HasPrefix(strings.TrimSpace(out), "ERROR") {
		return out, failure(out, stderr, code, what)
	}
	return out, nil
}

func sshPort(o Overview) int {
	if o.Me.SSHPort > 0 {
		return o.Me.SSHPort
	}
	return 22
}

func blockedErr(o Overview) error {
	who := "this computer"
	if o.Me.IP != "" {
		who += " (" + o.Me.IP + ")"
	}
	return fmt.Errorf("this would block SSH (port %d) from %s and cut the app off from the server", sshPort(o), who)
}

// keepsAccess checks that the app's own SSH connection still gets through rules: always when ufw is on;
// when it is off, that a change does not take away access the rules gave (Enable checks the rest).
func keepsAccess(o Overview, rules []FwRule) error {
	src := net.ParseIP(o.Me.IP)
	after := Verdict(rules, o.Firewall.DefaultIn, sshPort(o), "tcp", src)
	if after == Allowed {
		return nil
	}
	if o.Firewall.UfwActive || Verdict(o.Firewall.Rules, o.Firewall.DefaultIn, sshPort(o), "tcp", src) == Allowed {
		return blockedErr(o)
	}
	return nil
}

// AddRule adds a ufw rule. Deny and reject rules go first (ufw prepend) so they win over allow rules.
// A rule that would block the app's own SSH connection is refused.
func AddRule(ctx context.Context, h Host, o Overview, n NewRule) error {
	args, r, err := n.args()
	if err != nil {
		return err
	}
	first := r.Action == "deny" || r.Action == "reject"
	rules := append([]FwRule{}, o.Firewall.Rules...)
	if first {
		rules = append([]FwRule{r}, rules...)
	} else {
		rules = append(rules, r)
	}
	if err := keepsAccess(o, rules); err != nil {
		return err
	}
	// Blocking this computer's own address is refused even while ufw is off.
	if first && !isAny(r.From) && fromMatches(r.From, net.ParseIP(o.Me.IP)) && r.covers(sshPort(o), "tcp") {
		return blockedErr(o)
	}
	if first {
		args = append([]string{"prepend"}, args...)
	}
	_, err = runUfw(ctx, h, "Adding the firewall rule", args...)
	return err
}

// DeleteRule deletes the ufw rule listed as cmd (one of o.Firewall.Rules).
func DeleteRule(ctx context.Context, h Host, o Overview, cmd string) error {
	idx := -1
	for i, r := range o.Firewall.Rules {
		if r.Cmd == cmd {
			idx = i
		}
	}
	if idx < 0 {
		return errors.New("this rule is no longer in the firewall: refresh the list")
	}
	rules := append(append([]FwRule{}, o.Firewall.Rules[:idx]...), o.Firewall.Rules[idx+1:]...)
	if err := keepsAccess(o, rules); err != nil {
		return err
	}
	// The rule's own words, without its comment (ufw matches the rule itself).
	t := tokens(cmd)
	words := []string{"delete"}
	for i := 0; i < len(t); i++ {
		if t[i] == "comment" {
			i++
			continue
		}
		words = append(words, t[i])
	}
	_, err := runUfw(ctx, h, "Deleting the firewall rule", words...)
	return err
}

// Enable turns ufw on. When its rules would not let the app's SSH connection in, it first adds a rule
// allowing the SSH port; added says so.
func Enable(ctx context.Context, h Host, o Overview) (added string, err error) {
	if !o.Firewall.UfwInstalled {
		return "", errors.New("ufw is not installed on this server")
	}
	port := sshPort(o)
	src := net.ParseIP(o.Me.IP)
	if Verdict(o.Firewall.Rules, o.Firewall.DefaultIn, port, "tcp", src) != Allowed {
		allowSSH := FwRule{Action: "allow", Dir: "in", Proto: "tcp", From: "any", To: "any", Port: strconv.Itoa(port)}
		if Verdict(append(append([]FwRule{}, o.Firewall.Rules...), allowSSH), o.Firewall.DefaultIn, port, "tcp", src) != Allowed {
			return "", fmt.Errorf("a firewall rule blocks SSH (port %d) from this computer: delete it before turning the firewall on", port)
		}
		if _, err := runUfw(ctx, h, "Allowing SSH", "allow", "proto", "tcp", "from", "any", "to", "any", "port", strconv.Itoa(port), "comment", "SSH (added by Server Dashboard)"); err != nil {
			return "", err
		}
		added = fmt.Sprintf("Added a rule allowing SSH (port %d/tcp) so the app keeps its connection.", port)
	}
	_, err = runUfw(ctx, h, "Turning the firewall on", "--force", "enable")
	return added, err
}

// Disable turns ufw off (its rules are kept).
func Disable(ctx context.Context, h Host) error {
	_, err := runUfw(ctx, h, "Turning the firewall off", "disable")
	return err
}
