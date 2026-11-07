package security

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Logins sums up the SSH logins of the last days, from the system journal.
type Logins struct {
	Days        int          `json:"days"`
	NoJournal   bool         `json:"noJournal"` // no systemd journal on the server
	Limited     bool         `json:"limited"`   // the user may only read its own journal (no root, not in adm)
	Failed      int          `json:"failed"`    // failed attempts (one per connection)
	Failed24h   int          `json:"failed24h"`
	Accepted    int          `json:"accepted"`
	SourceCount int          `json:"sourceCount"` // addresses that failed
	Sources     []FailSource `json:"sources"`     // the addresses with the most failures (up to 200)
	Recent      []Login      `json:"recent"`      // successful logins, newest first (up to 100)
	AppKey      string       `json:"appKey"`      // fingerprint of the key the app's own connection used
}

type FailSource struct {
	IP        string   `json:"ip"`
	Attempts  int      `json:"attempts"`
	First     int64    `json:"first"` // unix ms
	Last      int64    `json:"last"`
	Users     []string `json:"users"` // user names tried (up to 8)
	UserCount int      `json:"userCount"`
}

type Login struct {
	Time        int64  `json:"time"` // unix ms
	User        string `json:"user"`
	IP          string `json:"ip"`
	Method      string `json:"method"`      // publickey, password, keyboard-interactive/pam
	Fingerprint string `json:"fingerprint"` // SHA256:… for a key
	KeyType     string `json:"keyType"`
	Mine        bool   `json:"mine"` // the app's own connection
}

// One failed attempt per connection (sshd process): a bot's connection logs "Invalid user", then "Failed
// password", then "Connection closed"; these count once. Lines are aggregated on the server, so a week of
// brute force does not travel over SSH.
const loginsAwk = `
function at(tok,   i) { for (i = 4; i <= NF; i++) if ($i == tok) return i; return 0 }
BEGIN { split(sc, me, " ") }
{
  pid = $3; sub(/^[^[]*\[/, "", pid); sub(/\].*$/, "", pid)
  t = int($1)
  if ($4 == "Accepted") {
    i = at("from"); if (!i) next
    fp = ""; kt = ""
    if ($NF ~ /^SHA256:/) { fp = $NF; kt = $(NF-1) }
    line = "A\t" t "\t" $5 "\t" $(i-1) "\t" $(i+1) "\t" fp "\t" kt
    if ($(i+1) == me[1] && $(i+3) == me[2]) print "M" substr(line, 2)
    acc[na % 100] = line; na++
    next
  }
  user = ""; ip = ""
  if ($4 == "Failed" || ($4 == "Invalid" && $5 == "user")) {
    i = at("from"); if (!i) next
    ip = $(i+1); user = $(i-1)
    if ($4 == "Invalid" && i == 6) user = ""
  } else if ($4 == "Connection" || $4 == "Disconnected") {
    i = at("port"); if (i < 7) next
    if ($(i-3) == "user" && ($(i-4) == "authenticating" || $(i-4) == "invalid")) { ip = $(i-1); user = $(i-2) }
    else if ($(i-2) == "user" && ($(i-3) == "authenticating" || $(i-3) == "invalid")) { ip = $(i-1); user = "" }
    else next
  } else next
  k = pid " " ip
  if (k in seen) next
  seen[k] = 1
  if (!(ip in n)) first[ip] = t
  n[ip]++; last[ip] = t; fails++
  if (t >= now - 86400) f24++
  u = ip SUBSEP user
  if (!(u in uu)) { uu[u] = 1; nu[ip]++; if (nu[ip] <= 8) users[ip] = users[ip] (nu[ip] > 1 ? "," : "") user }
}
END {
  print "T\t" fails + 0 "\t" f24 + 0 "\t" na + 0
  for (j = (na > 100 ? na - 100 : 0); j < na; j++) print acc[j % 100]
  for (ip in n) print "F\t" ip "\t" n[ip] "\t" first[ip] "\t" last[ip] "\t" nu[ip] "\t" users[ip]
}`

const loginsBody = `export LC_ALL=C PATH="$PATH:/usr/sbin:/sbin"
command -v journalctl >/dev/null 2>&1 || { echo @@NOJOURNAL; exit 0; }
if [ "$1" = 2 ]; then case " $(id -nG) " in *" adm "*|*" systemd-journal "*|*" wheel "*) ;; *) echo @@LIMITED ;; esac; fi
echo @@DATA
journalctl -q --no-pager -o short-unix --since "-$4d" -t sshd -t sshd-session -t sshd-auth 2>/dev/null |
  awk -v now="$(date +%s)" -v sc="$2" "$5"`

// ReadLogins reads the SSH logins of the last days (1 to 90).
func ReadLogins(ctx context.Context, h Host, days int) (Logins, error) {
	if days < 1 || days > 90 {
		days = 7
	}
	out, stderr, code, err := run(ctx, h, wrap(loginsBody, strconv.Itoa(days), loginsAwk), nil, 3*time.Minute)
	if err != nil {
		return Logins{Days: days}, err
	}
	if code != 0 {
		return Logins{Days: days}, failure(out, stderr, code, "Reading the SSH logins")
	}
	return parseLogins(out, days), nil
}

func parseLogins(out string, days int) Logins {
	me, rest := parseMe(out)
	s := sections(rest)
	lg := Logins{Days: days, Sources: []FailSource{}, Recent: []Login{}}
	_, lg.NoJournal = s["NOJOURNAL"]
	_, lg.Limited = s["LIMITED"]
	ms := func(v string) int64 { n, _ := strconv.ParseInt(v, 10, 64); return n * 1000 }
	for _, l := range s["DATA"] {
		f := strings.Split(l, "\t")
		switch {
		case f[0] == "T" && len(f) >= 4:
			lg.Failed, _ = strconv.Atoi(f[1])
			lg.Failed24h, _ = strconv.Atoi(f[2])
			lg.Accepted, _ = strconv.Atoi(f[3])
		case (f[0] == "A" || f[0] == "M") && len(f) >= 7:
			if f[0] == "M" {
				lg.AppKey = f[5]
				continue
			}
			lg.Recent = append(lg.Recent, Login{Time: ms(f[1]), Method: f[2], User: f[3], IP: f[4], Fingerprint: f[5], KeyType: f[6]})
		case f[0] == "F" && len(f) >= 7:
			src := FailSource{IP: f[1], First: ms(f[3]), Last: ms(f[4]), Users: []string{}}
			src.Attempts, _ = strconv.Atoi(f[2])
			src.UserCount, _ = strconv.Atoi(f[5])
			if f[6] != "" {
				src.Users = strings.Split(f[6], ",")
			}
			lg.Sources = append(lg.Sources, src)
		}
	}
	// Newest first; the app's own logins are those of its address and key (or of its address when it signs in otherwise).
	sort.SliceStable(lg.Recent, func(i, j int) bool { return lg.Recent[i].Time > lg.Recent[j].Time })
	for i := range lg.Recent {
		r := &lg.Recent[i]
		r.Mine = me.IP != "" && r.IP == me.IP && r.User == me.User && (lg.AppKey == "" || r.Fingerprint == lg.AppKey)
	}
	lg.SourceCount = len(lg.Sources)
	sort.Slice(lg.Sources, func(i, j int) bool {
		if lg.Sources[i].Attempts != lg.Sources[j].Attempts {
			return lg.Sources[i].Attempts > lg.Sources[j].Attempts
		}
		return lg.Sources[i].Last > lg.Sources[j].Last
	})
	if len(lg.Sources) > 200 {
		lg.Sources = lg.Sources[:200]
	}
	return lg
}
