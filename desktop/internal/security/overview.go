package security

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session is someone signed in to the server right now.
type Session struct {
	ID      string `json:"id"` // logind session id ("" when read from who)
	User    string `json:"user"`
	TTY     string `json:"tty"`     // pts/0, or "" for a session without a terminal (scp, sftp, tools)
	From    string `json:"from"`    // remote address, "" for a local console
	Service string `json:"service"` // sshd, login…
	State   string `json:"state"`   // active, online, closing
	Since   int64  `json:"since"`   // unix ms, 0 when unknown
	Mine    bool   `json:"mine"`    // the app's own connection
}

// SSHConfig is what sshd actually uses (sshd -T), read as root.
type SSHConfig struct {
	Known           bool   `json:"known"`
	Ports           []int  `json:"ports"`
	PasswordAuth    bool   `json:"passwordAuth"`
	KbdInteractive  bool   `json:"kbdInteractive"`
	PubkeyAuth      bool   `json:"pubkeyAuth"`
	PermitRootLogin string `json:"permitRootLogin"` // yes, no, prohibit-password, forced-commands-only
	PermitEmpty     bool   `json:"permitEmpty"`
	MaxAuthTries    int    `json:"maxAuthTries"`
}

// Overview is the Security tab's first screen.
type Overview struct {
	Me         Me         `json:"me"`
	Sessions   []Session  `json:"sessions"`
	SessionsBy string     `json:"sessionsBy"` // logind, who, or "" when neither is available
	SSH        SSHConfig  `json:"ssh"`
	Firewall   Firewall   `json:"firewall"`
	Exposed    []Exposure `json:"exposed"`
	Docker     bool       `json:"docker"` // Docker answered
}

const overviewBody = `export LC_ALL=C PATH="$PATH:/usr/sbin:/sbin"
R=$1; D=$4; [ "$R" = 2 ] || D=docker
echo @@SESSIONS
if command -v loginctl >/dev/null 2>&1 && loginctl list-sessions --no-legend >/dev/null 2>&1; then
  echo @@LOGIND
  for id in $(loginctl list-sessions --no-legend 2>/dev/null | awk '{print $1}'); do
    echo ---
    loginctl show-session "$id" -p Id -p Name -p TTY -p RemoteHost -p Remote -p Service -p Leader -p Class -p State 2>/dev/null
    l=$(loginctl show-session "$id" -p Leader --value 2>/dev/null)
    case $l in ''|0) ;; *) echo "Elapsed=$(ps -o etimes= -p "$l" 2>/dev/null | tr -d ' ')" ;; esac
  done
elif command -v who >/dev/null 2>&1; then
  echo @@WHO
  who -u 2>/dev/null | awk '{h=""; p=$NF; if ($NF ~ /^\(/) {h=$NF; p=$(NF-1)} print $1, $2, p, h}' | while read -r u t p h; do
    printf '%s\t%s\t%s\t%s\t%s\n' "$u" "$t" "$p" "$h" "$(ps -o etimes= -p "$p" 2>/dev/null | tr -d ' ')"
  done
fi
echo @@SSHD
sshd -T 2>/dev/null | grep -iE '^(port|passwordauthentication|permitrootlogin|pubkeyauthentication|kbdinteractiveauthentication|permitemptypasswords|maxauthtries) '
echo @@UFW
if command -v ufw >/dev/null 2>&1; then
  echo installed
  ufw status 2>&1 | head -n 1
  echo @@UFW_ADDED
  ufw show added 2>&1
  echo @@UFW_DEFAULT
  grep -E '^DEFAULT_INPUT_POLICY=' /etc/default/ufw 2>/dev/null
fi
echo @@IPT
if command -v iptables >/dev/null 2>&1; then iptables -S 2>/dev/null | grep -E '^-(P|A) (INPUT|DOCKER-USER) '; fi
echo @@NFT
if command -v nft >/dev/null 2>&1; then
  nft list ruleset 2>/dev/null | awk '
/^table /{t=$2 " " $3; next}
/^[ \t]*chain /{c=$2; inch=1; hook=0; pol=""; n=0; next}
inch && /hook input/{hook=1; if (match($0, /policy [a-z]+/)) pol=substr($0, RSTART+7, RLENGTH-7); next}
inch && /^[ \t]*}/{if (hook) print t "\t" c "\t" pol "\t" n; inch=0; next}
inch && hook && NF {n++}'
fi
echo @@DOCKER
if command -v docker >/dev/null 2>&1; then $D ps --format '{{.Names}}\t{{.Ports}}' 2>/dev/null && echo @@DOCKER_OK; fi
echo @@LISTEN
ss -H -tln 2>/dev/null
`

// ReadOverview reads sessions, the SSH settings, the firewall and the ports open to the network.
// docker is the docker command for a user that is not root ("docker" or "sudo -n docker").
func ReadOverview(ctx context.Context, h Host, docker string) (Overview, error) {
	out, stderr, code, err := run(ctx, h, wrap(overviewBody, docker), nil, 60*time.Second)
	if err != nil {
		return Overview{}, err
	}
	if code != 0 && !strings.Contains(out, "@@LISTEN") {
		return Overview{}, failure(out, stderr, code, "Reading the security settings")
	}
	return parseOverview(out, time.Now()), nil
}

func parseOverview(out string, now time.Time) Overview {
	me, rest := parseMe(out)
	s := sections(rest)
	o := Overview{Me: me, Sessions: []Session{}, Exposed: []Exposure{}}
	if _, ok := s["LOGIND"]; ok {
		o.SessionsBy = "logind"
		o.Sessions = parseLogind(s["LOGIND"], me.SessionID, now)
	} else if _, ok := s["WHO"]; ok {
		o.SessionsBy = "who"
		o.Sessions = parseWho(s["WHO"], now)
	}
	o.SSH = parseSSHD(s["SSHD"])
	if o.Me.SSHPort == 0 && len(o.SSH.Ports) > 0 {
		o.Me.SSHPort = o.SSH.Ports[0]
	}
	o.Firewall = parseFirewall(s, me.Root)
	_, o.Docker = s["DOCKER_OK"]
	o.Exposed = exposures(o.Firewall, s["DOCKER"], s["LISTEN"], o.SSH.Ports, me.Root)
	return o
}

// ---------------------------------------------------------------- sessions

// User sessions only: not the per-user service manager, greeters or background jobs.
var userClasses = map[string]bool{"": true, "user": true, "user-early": true, "user-incomplete": true}

func parseLogind(lines []string, self string, now time.Time) []Session {
	out := []Session{}
	var cur map[string]string
	flush := func() {
		if cur == nil || cur["Id"] == "" || !userClasses[cur["Class"]] {
			return
		}
		ses := Session{ID: cur["Id"], User: cur["Name"], TTY: cur["TTY"], From: cur["RemoteHost"], Service: cur["Service"], State: cur["State"]}
		if el, err := strconv.ParseInt(cur["Elapsed"], 10, 64); err == nil {
			ses.Since = now.Add(-time.Duration(el) * time.Second).UnixMilli()
		}
		ses.Mine = self != "" && ses.ID == self
		out = append(out, ses)
	}
	for _, l := range lines {
		if l == "---" {
			flush()
			cur = map[string]string{}
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok && cur != nil {
			cur[k] = v
		}
	}
	flush()
	sortSessions(out)
	return out
}

func parseWho(lines []string, now time.Time) []Session {
	out := []Session{}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) < 5 || f[0] == "" {
			continue
		}
		ses := Session{User: f[0], TTY: f[1], From: strings.Trim(f[3], "()"), Service: "", State: "active"}
		if el, err := strconv.ParseInt(f[4], 10, 64); err == nil {
			ses.Since = now.Add(-time.Duration(el) * time.Second).UnixMilli()
		}
		if strings.HasPrefix(ses.From, ":") { // X display, not a remote address
			ses.From = ""
		}
		out = append(out, ses)
	}
	sortSessions(out)
	return out
}

func sortSessions(list []Session) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Since != list[j].Since {
			return list[i].Since > list[j].Since // newest first
		}
		return list[i].ID < list[j].ID
	})
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
var ttyRe = regexp.MustCompile(`^(pts/[0-9]{1,5}|tty[0-9]{1,3})$`)

// EndSession signs a user out: the logind session (id), or every process on a terminal (tty) when the server
// has no logind. The app's own session is refused.
func EndSession(ctx context.Context, h Host, id, tty string) error {
	var body, arg string
	switch {
	case id != "":
		if !sessionIDRe.MatchString(id) {
			return errors.New("invalid session")
		}
		body, arg = `[ "$3" = "$4" ] && { echo "this is the app's own session" >&2; exit 4; }
loginctl terminate-session "$4"`, id
	case tty != "":
		if !ttyRe.MatchString(tty) {
			return errors.New("invalid terminal")
		}
		body, arg = `pkill -KILL -t "$4"`, tty
	default:
		return errors.New("no session given")
	}
	out, stderr, code, err := run(ctx, h, wrap(body, arg), nil, 30*time.Second)
	if err != nil {
		return err
	}
	if code == 4 {
		return errors.New("this is the app's own connection: it cannot end itself")
	}
	if code != 0 {
		return failure(out, stderr, code, "Ending the session")
	}
	return nil
}

// ---------------------------------------------------------------- sshd

func parseSSHD(lines []string) SSHConfig {
	c := SSHConfig{Ports: []int{}}
	for _, l := range lines {
		k, v, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok {
			continue
		}
		c.Known = true
		v = strings.ToLower(strings.TrimSpace(v))
		switch strings.ToLower(k) {
		case "port":
			if p, err := strconv.Atoi(v); err == nil {
				c.Ports = append(c.Ports, p)
			}
		case "passwordauthentication":
			c.PasswordAuth = v == "yes"
		case "kbdinteractiveauthentication":
			c.KbdInteractive = v == "yes"
		case "pubkeyauthentication":
			c.PubkeyAuth = v == "yes"
		case "permitrootlogin":
			c.PermitRootLogin = v
		case "permitemptypasswords":
			c.PermitEmpty = v == "yes"
		case "maxauthtries":
			c.MaxAuthTries, _ = strconv.Atoi(v)
		}
	}
	return c
}
