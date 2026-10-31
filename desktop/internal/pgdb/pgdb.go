// Package pgdb backs up, restores and copies PostgreSQL databases that run in Docker containers.
//
// Everything runs inside the database container (docker exec), so pg_dump and pg_restore always match
// the server version and no client tools are needed on the servers or on this computer:
//
//	server A: pg_dump -Fd -j N (parallel, compressed)  →  tar  ─┐
//	this computer: relays the stream (and can keep a copy)     ─┤
//	server B: tar  →  pg_restore -j N                          ←┘
//
// The two servers never need to reach each other.
package pgdb

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Host runs a shell command on a server (over SSH in the app, locally in tests).
// A command that exits non-zero returns an *ExitError.
type Host interface {
	Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error
}

type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

// Side is one end of a job: a server and the docker command to use on it.
type Side struct {
	Host    Host
	Docker  string // "docker" or "sudo -n docker"
	Server  string // display name
	Address string // host name or IP the server is reached at (default source address of a direct transfer)
}

var (
	containerRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// Images that run PostgreSQL. Anything else is ignored when looking for databases.
	imageRe = regexp.MustCompile(`(?i)postgres|postgis|timescale|pgvector|paradedb|supabase/postgres|pgautoupgrade|citus|bitnami/postgresql`)
)

func validContainer(c string) error {
	if !containerRe.MatchString(c) {
		return errors.New("invalid container name")
	}
	return nil
}

// ValidDatabase accepts the names PostgreSQL allows, minus control characters and "=" (which would
// make pg_dump read the name as a connection string).
func ValidDatabase(name string) error {
	if name == "" || len(name) > 63 || strings.ContainsAny(name, "=\x00\r\n\t") {
		return errors.New("invalid database name")
	}
	return nil
}

// quote makes a string safe as one shell word.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ident quotes an SQL identifier; lit quotes an SQL string literal.
func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func lit(s string) string   { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// dexec builds `docker exec [-i] <container> sh -c '<script>'`.
func dexec(docker, container string, stdin bool, script string) string {
	flag := ""
	if stdin {
		flag = "-i "
	}
	return fmt.Sprintf("%s exec %s%s sh -c %s", docker, flag, container, quote(script))
}

// pgEnv is the start of every script run in a container: it logs in the way the image was set up,
// with the user and password from the container's own environment (official and Bitnami images),
// so no password ever travels on a command line. T is a tab, for psql -F.
const pgEnv = `export PGUSER="${PGUSER:-${POSTGRES_USER:-${POSTGRESQL_USERNAME:-postgres}}}"
if [ -z "$PGPASSWORD" ]; then
  if [ -n "$POSTGRES_PASSWORD" ]; then export PGPASSWORD="$POSTGRES_PASSWORD"
  elif [ -n "$POSTGRES_PASSWORD_FILE" ] && [ -r "$POSTGRES_PASSWORD_FILE" ]; then export PGPASSWORD="$(cat "$POSTGRES_PASSWORD_FILE")"
  elif [ -n "$POSTGRESQL_PASSWORD" ]; then export PGPASSWORD="$POSTGRESQL_PASSWORD"; fi
fi
T="$(printf '\t')"
`

// run executes a command and returns its stdout; a non-zero exit becomes an error with the command's message.
func run(ctx context.Context, h Host, cmd string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out, errb strings.Builder
	err := h.Run(ctx, cmd, nil, &out, &errb)
	if err != nil {
		var ee *ExitError
		if errors.As(err, &ee) {
			msg := strings.TrimSpace(errb.String() + "\n" + out.String())
			if msg == "" {
				msg = err.Error()
			}
			return out.String(), errors.New(friendly(msg))
		}
		if ctx.Err() == context.DeadlineExceeded {
			return out.String(), fmt.Errorf("timed out after %v", timeout)
		}
		return out.String(), err
	}
	return out.String(), nil
}

// friendly turns common failures into plain words.
func friendly(msg string) string {
	switch {
	case strings.Contains(msg, "permission denied while trying to connect to the Docker daemon"):
		return "this SSH user may not use Docker: add it to the docker group, or tick “Run docker with sudo -n” for this server"
	case strings.Contains(msg, "password authentication failed"):
		return "PostgreSQL refused the login (password authentication failed). The container's POSTGRES_USER / POSTGRES_PASSWORD were used"
	case strings.Contains(msg, "No such container"):
		return "the container no longer exists"
	case strings.Contains(msg, "is not running"):
		return "the container is not running"
	}
	return lastLines(msg, 6)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ------------------------------------------------------------------ instances

// Database is one database of an instance.
type Database struct {
	Name     string `json:"name"`
	Bytes    int64  `json:"bytes"` // -1 when the user may not connect to it
	Encoding string `json:"encoding"`
	Collate  string `json:"collate"`
	Ctype    string `json:"ctype"`
}

// Instance is a PostgreSQL server running in a container.
type Instance struct {
	Container string     `json:"container"`
	Image     string     `json:"image"`
	Version   int        `json:"version"` // server_version_num, e.g. 160004
	User      string     `json:"user"`
	Superuser bool       `json:"superuser"`
	CreateDB  bool       `json:"createdb"`
	MaintDB   string     `json:"maintDb"` // database to connect to for CREATE/DROP DATABASE
	Cores     int        `json:"cores"`
	MemKB     int64      `json:"memKb"`
	TmpFreeKB int64      `json:"tmpFreeKb"`  // free space for the dump folder
	DataFree  int64      `json:"dataFreeKb"` // free space where the databases are stored
	MaxConn   int        `json:"maxConn"`
	Conns     int        `json:"conns"`
	PgDump    string     `json:"pgDump"`
	Databases []Database `json:"databases"`
	Error     string     `json:"error,omitempty"` // could not log in or query
}

// Major returns the major version (16 for 160004, 9 for 90624).
func (i Instance) Major() int { return i.Version / 10000 }

// VersionText formats the version as "16.4".
func VersionText(v int) string {
	if v <= 0 {
		return "?"
	}
	if v >= 100000 {
		return fmt.Sprintf("%d.%d", v/10000, v%10000)
	}
	return fmt.Sprintf("%d.%d.%d", v/10000, v/100%100, v%100)
}

func (i Instance) Database(name string) (Database, bool) {
	for _, d := range i.Databases {
		if d.Name == name {
			return d, true
		}
	}
	return Database{}, false
}

const probeScript = pgEnv + `MDB=postgres
psql -XAtq -d postgres -c 'select 1' >/dev/null 2>&1 || MDB="${POSTGRES_DB:-template1}"
echo "mdb=$MDB"
echo "user=$PGUSER"
echo "nproc=$(nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo)"
echo "mem=$(awk '/^MemTotal/{print $2}' /proc/meminfo 2>/dev/null)"
echo "tmpfree=$(df -Pk /tmp 2>/dev/null | awk 'NR==2{print $4}')"
echo "datafree=$(df -Pk "${PGDATA:-/var/lib/postgresql/data}" 2>/dev/null | awk 'NR==2{print $4}')"
echo "pgdump=$(pg_dump --version 2>&1 | head -n 1)"
echo "@@SQL"
psql -XAtq -v ON_ERROR_STOP=1 -F "$T" -d "$MDB" 2>&1 <<'SQL'
select 'version', current_setting('server_version_num');
select 'super', rolsuper::text, rolcreatedb::text from pg_roles where rolname = current_user;
select 'maxconn', current_setting('max_connections');
select 'conns', count(*)::text from pg_stat_activity;
select 'db', datname,
  case when has_database_privilege(datname, 'CONNECT') then pg_database_size(datname) else -1 end,
  pg_encoding_to_char(encoding), datcollate, datctype
from pg_database where datallowconn and not datistemplate order by datname;
SQL
`

// FindInstances lists the running containers with a PostgreSQL image and what is inside each.
func FindInstances(ctx context.Context, s Side) ([]Instance, error) {
	out, err := run(ctx, s.Host, s.Docker+` ps --no-trunc --format '{{.Names}}\t{{.Image}}'`, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var list []Instance
	for _, line := range strings.Split(out, "\n") {
		name, image, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && imageRe.MatchString(image) && containerRe.MatchString(name) {
			list = append(list, Instance{Container: name, Image: image})
		}
	}
	if len(list) == 0 {
		return []Instance{}, nil
	}
	// One SSH command probes every candidate.
	var script strings.Builder
	for _, in := range list {
		fmt.Fprintf(&script, "echo @@C %s; %s 2>&1\n", in.Container, dexec(s.Docker, in.Container, false, probeScript))
	}
	out, err = run(ctx, s.Host, script.String(), 60*time.Second)
	if err != nil && out == "" {
		return nil, err
	}
	blocks := map[string]string{}
	cur := ""
	for _, line := range strings.SplitAfter(out, "\n") {
		if strings.HasPrefix(line, "@@C ") {
			cur = strings.TrimSpace(line[4:])
			continue
		}
		blocks[cur] += line
	}
	for i := range list {
		parseProbe(&list[i], blocks[list[i].Container])
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Container < list[b].Container })
	return list, nil
}

// Probe reads one container.
func Probe(ctx context.Context, s Side, container string) (Instance, error) {
	if err := validContainer(container); err != nil {
		return Instance{}, err
	}
	in := Instance{Container: container}
	out, err := run(ctx, s.Host, dexec(s.Docker, container, false, probeScript)+" 2>&1", 60*time.Second)
	if err != nil && out == "" {
		return in, err
	}
	parseProbe(&in, out)
	if in.Error != "" {
		return in, errors.New(in.Error)
	}
	return in, nil
}

func parseProbe(in *Instance, out string) {
	head, sql, found := strings.Cut(out, "@@SQL\n")
	if !found {
		in.Error = friendly(strings.TrimSpace(out))
		if in.Error == "" {
			in.Error = "no answer from the container"
		}
		return
	}
	for _, line := range strings.Split(head, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "mdb":
			in.MaintDB = v
		case "user":
			in.User = v
		case "nproc":
			in.Cores, _ = strconv.Atoi(v)
		case "mem":
			in.MemKB, _ = strconv.ParseInt(v, 10, 64)
		case "tmpfree":
			in.TmpFreeKB, _ = strconv.ParseInt(v, 10, 64)
		case "datafree":
			in.DataFree, _ = strconv.ParseInt(v, 10, 64)
		case "pgdump":
			in.PgDump = v
		}
	}
	in.Databases = []Database{}
	var junk []string
	for _, line := range strings.Split(sql, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch {
		case f[0] == "version" && len(f) == 2:
			in.Version, _ = strconv.Atoi(f[1])
		case f[0] == "super" && len(f) == 3:
			in.Superuser, in.CreateDB = f[1] == "true", f[2] == "true"
		case f[0] == "maxconn" && len(f) == 2:
			in.MaxConn, _ = strconv.Atoi(f[1])
		case f[0] == "conns" && len(f) == 2:
			in.Conns, _ = strconv.Atoi(f[1])
		case f[0] == "db" && len(f) == 6:
			size, _ := strconv.ParseInt(f[2], 10, 64)
			in.Databases = append(in.Databases, Database{Name: f[1], Bytes: size, Encoding: f[3], Collate: f[4], Ctype: f[5]})
		case strings.TrimSpace(line) != "":
			junk = append(junk, line)
		}
	}
	if in.Version == 0 {
		in.Error = friendly(strings.Join(junk, "\n"))
		if in.Error == "" {
			in.Error = "could not query PostgreSQL"
		}
	}
}

// ------------------------------------------------------------------ table sizes

// Table is a table with data to copy and its size on disk (with TOAST, without indexes).
type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
}

func (t Table) Key() string { return t.Schema + "." + t.Name }

const tablesSQL = `select n.nspname, c.relname, pg_table_size(c.oid)
from pg_class c join pg_namespace n on n.oid = c.relnamespace
where c.relkind = 'r' and c.relpersistence <> 't'
  and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg_toast%' and n.nspname not like 'pg_temp%'
order by 3 desc;`

func listTables(ctx context.Context, s Side, container, db string) ([]Table, error) {
	script := pgEnv + "psql -XAtq -v ON_ERROR_STOP=1 -F \"$T\" -d " + quote(db) + " <<'SQL'\n" + tablesSQL + "\nSQL\n"
	out, err := run(ctx, s.Host, dexec(s.Docker, container, false, script), 2*time.Minute)
	if err != nil {
		return nil, err
	}
	tables := []Table{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) != 3 {
			continue
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		tables = append(tables, Table{Schema: f[0], Name: f[1], Bytes: size})
	}
	return tables, nil
}

// ------------------------------------------------------------------ helpers

// lineWriter calls fn for every complete line written to it.
type lineWriter struct {
	buf []byte
	fn  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		w.fn(line)
	}
	if len(w.buf) > 64*1024 { // a runaway line without a newline
		w.fn(string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// scanLines splits text into trimmed, non-empty lines.
func scanLines(s string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
