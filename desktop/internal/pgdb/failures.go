package pgdb

// Statements that failed during a restore: read from pg_restore's output, explained in plain words, and run
// again on request (once the cause is fixed, or with safer settings).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// FailedStatement is one statement pg_restore could not run.
type FailedStatement struct {
	Object  string `json:"object,omitempty"`  // what it creates, e.g. "INDEX ix_chunks_embedding"
	Error   string `json:"error"`             // PostgreSQL's message
	Detail  string `json:"detail,omitempty"`  // its DETAIL / HINT / LINE lines
	Command string `json:"command,omitempty"` // the statement as pg_restore ran it
	Title   string `json:"title"`             // what went wrong, in plain words
	Fix     string `json:"fix,omitempty"`     // what to do about it
	Again   string `json:"again,omitempty"`   // what Retry does differently, when it helps by itself
	Retry   bool   `json:"retry"`             // the statement can be run again
	Status  string `json:"status,omitempty"`  // "" (failed during the restore), "fixed", "failed" (failed again on retry)
}

const (
	maxFailed  = 60
	maxCommand = 256 << 10
)

var tocEntryRe = regexp.MustCompile(`from TOC entry \d+; \d+ \d+ (.+) \S+$`)

// failureParser follows pg_restore's stderr. An error looks like
//
//	pg_restore: from TOC entry 4219; 1259 16405 INDEX ix_chunks_embedding app
//	pg_restore: error: could not execute query: ERROR:  could not resize shared memory segment …
//	Command was: CREATE INDEX ix_chunks_embedding ON public.chunks USING hnsw (…);
//
// where the message and the command may span several lines (those do not start with "pg_restore:").
type failureParser struct {
	items     []FailedStatement
	cur       *FailedStatement
	object    string // the TOC entry announced before the error
	inCmd     bool
	cmd       strings.Builder
	truncated bool
	dropped   int
}

func (p *failureParser) line(l string) {
	prefixed := strings.HasPrefix(l, "pg_restore:")
	if prefixed {
		p.close()
	}
	trimmed := strings.TrimSpace(l)
	switch {
	case prefixed && strings.Contains(l, "from TOC entry "):
		if m := tocEntryRe.FindStringSubmatch(l); m != nil {
			p.object = m[1]
		}
	case prefixed && strings.Contains(l, "could not execute query:"):
		msg := strings.TrimSpace(l[strings.Index(l, "could not execute query:")+len("could not execute query:"):])
		p.open(stripSeverity(msg))
	case prefixed && strings.Contains(l, "error:"):
		p.open(strings.TrimSpace(l[strings.Index(l, "error:")+len("error:"):]))
	case prefixed:
		p.object = ""
	case p.cur == nil:
	case p.inCmd:
		p.addCmd("\n" + l)
	case strings.HasPrefix(trimmed, "Command was:"):
		p.inCmd = true
		p.addCmd(strings.TrimSpace(strings.TrimPrefix(trimmed, "Command was:")))
	case trimmed != "": // DETAIL, HINT, LINE n and the caret under it
		if p.cur.Detail != "" {
			p.cur.Detail += "\n"
		}
		p.cur.Detail += l
	}
}

func stripSeverity(msg string) string {
	for _, s := range []string{"ERROR:", "FATAL:"} {
		if strings.HasPrefix(msg, s) {
			return strings.TrimSpace(msg[len(s):])
		}
	}
	return msg
}

func (p *failureParser) open(msg string) {
	p.cur = &FailedStatement{Object: p.object, Error: msg}
	p.object = ""
}

func (p *failureParser) addCmd(s string) {
	if p.cmd.Len()+len(s) > maxCommand {
		p.truncated = true
		return
	}
	p.cmd.WriteString(s)
}

// close ends the error being read.
func (p *failureParser) close() {
	if p.cur == nil {
		return
	}
	f := *p.cur
	f.Command = strings.TrimSpace(p.cmd.String())
	explainFailure(&f)
	if p.truncated {
		f.Retry = false
	}
	if len(p.items) < maxFailed {
		p.items = append(p.items, f)
	} else {
		p.dropped++
	}
	p.cur, p.inCmd, p.truncated = nil, false, false
	p.cmd.Reset()
}

// result returns the errors read (call once the output has ended).
func (p *failureParser) result() []FailedStatement {
	p.close()
	return p.items
}

var (
	roleMissingRe = regexp.MustCompile(`role "([^"]+)" does not exist`)
	extMissingRe  = regexp.MustCompile(`extension "([^"]+)" is not available|could not open extension control file "[^"]*/([^"/]+)\.control"`)
	typeMissingRe = regexp.MustCompile(`type "([^"]+)" does not exist`)
)

// Failed records a new error of the statement (after a retry) and explains it.
func (f *FailedStatement) Failed(msg string) {
	f.Error, f.Detail, f.Status = msg, "", "failed"
	explainFailure(f)
}

// explainFailure fills Title, Fix and Retry from the message.
func explainFailure(f *FailedStatement) {
	f.Retry = f.Command != ""
	var e Explanation
	f.Title, e = explain(f.Error)
	f.Fix, f.Again = e.Fix, e.Again
	if strings.Contains(f.Error, "already exists") {
		f.Retry = false
	}
}

// Explanation says how to fix an error, and what running the statement again does by itself.
type Explanation struct {
	Fix   string
	Again string
}

// Explain says in plain words what a PostgreSQL error means and how to fix it. The title is the message
// itself when it is not a known case; fix is then empty.
func Explain(msg string) (title, fix string) {
	title, e := explain(msg)
	return title, e.Fix
}

func explain(msg string) (string, Explanation) {
	type x = Explanation
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "could not resize shared memory segment") ||
		strings.Contains(low, "shared memory") && strings.Contains(low, "no space left on device"):
		return "The container ran out of shared memory (/dev/shm)", x{
			"Docker gives a container 64 MB of /dev/shm unless told otherwise, too little to build this index with parallel workers. " +
				"To avoid it next time, give the PostgreSQL container more: shm_size: 1gb in docker-compose.yml, or docker run --shm-size=1g, then recreate the container.",
			"Retry builds it again without parallel workers, which needs no /dev/shm."}
	case strings.Contains(low, "no space left on device"):
		return "The disk of the destination server is full", x{
			"Free some disk space on the destination server (old backups, logs, docker system prune).", ""}
	case strings.Contains(low, "out of memory"):
		return "PostgreSQL ran out of memory", x{
			"Give the container more memory, or use fewer workers (Speed: Gentle).",
			"Retry runs the statement alone, with less memory and without parallel workers."}
	case roleMissingRe.MatchString(msg):
		role := roleMissingRe.FindStringSubmatch(msg)[1]
		return fmt.Sprintf("The role %s does not exist on the destination", role), x{
			"Create the role " + role + " on the destination, or run the job again with “Keep them, and create the missing roles” " +
				"(or “Owned by the login of the destination”) under Owners, permissions and roles.", ""}
	case extMissingRe.MatchString(msg):
		m := extMissingRe.FindStringSubmatch(msg)
		ext := m[1] + m[2]
		hint := ""
		switch ext {
		case "vector":
			hint = " For pgvector, use an image such as pgvector/pgvector:pg16."
		case "postgis", "postgis_topology", "postgis_raster":
			hint = " For PostGIS, use an image such as postgis/postgis."
		case "timescaledb":
			hint = " For TimescaleDB, use an image such as timescale/timescaledb."
		}
		return fmt.Sprintf("The extension %s is not installed in the destination's PostgreSQL", ext), x{
			"The destination container's image lacks this extension, so the objects that use it fail too." + hint +
				" Once the image has it, restore the database again.", ""}
	case typeMissingRe.MatchString(msg):
		return fmt.Sprintf("The type %s does not exist on the destination", typeMissingRe.FindStringSubmatch(msg)[1]), x{
			"It usually comes from an extension that could not be created (see the other errors): fix that one first.", ""}
	case strings.Contains(low, "permission denied") || strings.Contains(low, "must be owner") || strings.Contains(low, "must be superuser"):
		return "The destination's PostgreSQL login is not allowed to do this", x{
			"Restore with a superuser login (the container's POSTGRES_USER), or choose “Owned by the login of the destination” under Owners, permissions and roles.", ""}
	case strings.Contains(low, "already exists"):
		return "The object already exists on the destination", x{
			"Usually harmless: for example the public schema, or an extension the destination already has. Nothing to do.", ""}
	case strings.Contains(low, "could not create unique index") || strings.Contains(low, "duplicate key"):
		return "Duplicate values prevent a unique index or constraint", x{
			"The data has rows with the same key: remove the duplicates on the destination.", ""}
	case strings.Contains(low, "violates foreign key constraint"):
		return "Some rows refer to rows that are missing (foreign key)", x{
			"Usually the rows of the referenced table were skipped (Skip the rows of these tables): copy that table too, or leave the constraint out.", ""}
	case strings.Contains(low, "statement timeout"):
		return "The statement took longer than the server allows (statement_timeout)", x{
			"Raise statement_timeout for the restore login.", "Retry runs it without a time limit."}
	case strings.Contains(low, "deadlock detected"):
		return "Two parallel restore workers blocked each other (deadlock)", x{
			"Use fewer workers (Speed: Gentle).", "Retry runs the statement on its own."}
	case strings.Contains(low, "too many connections") || strings.Contains(low, "remaining connection slots"):
		return "The destination has no free connections left", x{
			"Use fewer workers (Speed: Gentle), or raise max_connections on the destination.", ""}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return capital(first), x{}
}

// ------------------------------------------------------------------ retry

// RetryStatements runs failed statements again in database db, one at a time and in order, with the session
// settings pg_restore uses plus some that avoid the usual causes: no parallel index workers when /dev/shm is too
// small, no statement timeout. onResult gets each outcome (nil when the statement succeeded). appName names the
// connections, so a cancelled retry can stop the statement on the server.
func RetryStatements(ctx context.Context, side *Side, container, db, appName string, items []FailedStatement, onResult func(i int, err error)) error {
	in, err := Probe(ctx, *side, container)
	if err != nil {
		return err
	}
	if _, ok := in.Database(db); !ok {
		return fmt.Errorf("the database %s no longer exists on %s", db, side.Server)
	}
	mwm, noParallel := indexSettings(in, 1)
	opts := fmt.Sprintf("-c maintenance_work_mem=%dMB -c statement_timeout=0", mwm)
	if noParallel {
		opts += " -c max_parallel_maintenance_workers=0"
	}
	script := pgEnv + "export PGAPPNAME=" + quote(appName) + " PGOPTIONS=" + quote(opts) + "\nexec psql -XAtq -v ON_ERROR_STOP=1 -d " + quote(db)
	cmd := dexec(side.Docker, container, true, script)
	const session = "SET lock_timeout = 0;\nSET standard_conforming_strings = on;\nSELECT pg_catalog.set_config('search_path', '', false);\n" +
		"SET check_function_bodies = false;\nSET client_min_messages = warning;\n"
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			stopStatements(side, container, in.MaintDB, appName)
			return err
		}
		var out, errb strings.Builder
		err := side.Host.Run(ctx, cmd, strings.NewReader(session+it.Command+"\n"), &out, &errb)
		if ctx.Err() != nil {
			stopStatements(side, container, in.MaintDB, appName)
			return ctx.Err()
		}
		if err != nil {
			err = errors.New(psqlError(errb.String(), err))
		}
		onResult(i, err)
	}
	return nil
}

// stopStatements ends the connections named appName (closing SSH does not stop docker exec).
func stopStatements(side *Side, container, maint, appName string) {
	if maint == "" {
		maint = "postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script := pgEnv + "psql -XAtq -d " + quote(maint) + " -c " +
		quote("select pg_terminate_backend(pid) from pg_stat_activity where application_name = "+lit(appName)) + " >/dev/null 2>&1\ntrue"
	_, _ = run(ctx, side.Host, dexec(side.Docker, container, false, script), 30*time.Second)
}

// psqlError takes PostgreSQL's message out of psql's stderr ("psql:<stdin>:7: ERROR:  …").
func psqlError(stderr string, err error) string {
	var msg []string
	for _, l := range strings.Split(strings.TrimSpace(stderr), "\n") {
		l = strings.TrimRight(l, "\r")
		if i := strings.Index(l, "ERROR:"); i >= 0 {
			msg = append(msg, strings.TrimSpace(l[i+len("ERROR:"):]))
		} else if i := strings.Index(l, "FATAL:"); i >= 0 {
			msg = append(msg, strings.TrimSpace(l[i+len("FATAL:"):]))
		} else if len(msg) > 0 && l != "" {
			msg = append(msg, l)
		}
	}
	if len(msg) == 0 {
		if s := strings.TrimSpace(stderr); s != "" {
			return friendly(s)
		}
		return err.Error()
	}
	return strings.Join(msg, "\n")
}

// ------------------------------------------------------------------ index memory

// indexSettings chooses the memory of index builds (maintenance_work_mem, in MB) when jobs of them may run at
// once, and whether parallel index workers must be turned off. Parallel workers share their memory through
// /dev/shm (dynamic_shared_memory_type posix, the default), which Docker limits to 64 MB unless told otherwise:
// a bigger build then fails with "could not resize shared memory segment … No space left on device".
func indexSettings(in Instance, jobs int) (mwm int64, noParallel bool) {
	jobs = max(jobs, 1)
	mwm = 256
	if in.MemKB > 0 {
		mwm = min(max(in.MemKB/1024/int64(4*jobs), 64), 1024)
	}
	if in.ShmKB > 0 && (in.DSMType == "" || in.DSMType == "posix") {
		// Each running build may hold about its maintenance_work_mem in /dev/shm, plus some room for the rest.
		need := mwm*int64(jobs) + 32
		noParallel = in.ShmKB/1024 < need
	}
	return mwm, noParallel
}
