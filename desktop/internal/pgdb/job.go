package pgdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Endpoint is a database on a server: where to read from or write to.
type Endpoint struct {
	ServerID  string `json:"serverId"`
	Server    string `json:"server"` // display name, filled in by the app
	Container string `json:"container"`
	Database  string `json:"database"`
}

// Spec says what a job does.
type Spec struct {
	Kind        string   `json:"kind"` // copy (A → B), backup (A → file on this computer), restore (file → B)
	Source      Endpoint `json:"source"`
	Target      Endpoint `json:"target"`
	File        string   `json:"file"`        // backup: where to save; copy: optional local copy; restore: the file to read
	IfExists    string   `json:"ifExists"`    // target database exists: stop, replace (dropped after a good restore), keep (renamed)
	Jobs        int      `json:"jobs"`        // parallel jobs of the backup (and of the restore unless RestoreJobs is set), 0 = auto
	RestoreJobs int      `json:"restoreJobs"` // parallel jobs of the restore, 0 = like Jobs
	Compression string   `json:"compression"` // auto, zstd, lz4, gzip, none
	KeepOwners  bool     `json:"keepOwners"`  // restore owners and grants (the roles must exist on the target)
	SchemaOnly  bool     `json:"schemaOnly"`
	ExcludeData []string `json:"excludeData"` // table patterns (pg_dump -T syntax) whose rows are skipped
	Analyze     bool     `json:"analyze"`     // update planner statistics after the restore
	Verify      bool     `json:"verify"`      // compare exact row counts of every table (copy only)
	Direct      bool     `json:"direct"`      // copy: the target reads the source database over the network (see direct.go)
	DirectHost  string   `json:"directHost"`  // address of the source as the target sees it; "" = the source's SSH address
	DirectPort  int      `json:"directPort"`  // 0 = the port publishing the source container's 5432
}

func (s Spec) hasSource() bool { return s.Kind == "copy" || s.Kind == "backup" }
func (s Spec) hasTarget() bool { return s.Kind == "copy" || s.Kind == "restore" }

// restoreWant is the number of restore jobs asked for (0 = auto).
func (s Spec) restoreWant() int {
	if s.RestoreJobs > 0 {
		return s.RestoreJobs
	}
	return s.Jobs
}

// Validate checks a spec before anything runs.
func (s *Spec) Validate() error {
	switch s.Kind {
	case "copy", "backup", "restore":
	default:
		return errors.New("unknown job kind")
	}
	if s.hasSource() {
		if err := validContainer(s.Source.Container); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if err := ValidDatabase(s.Source.Database); err != nil {
			return fmt.Errorf("source: %w", err)
		}
	}
	if s.hasTarget() {
		if err := validContainer(s.Target.Container); err != nil {
			return fmt.Errorf("target: %w", err)
		}
		if err := ValidDatabase(s.Target.Database); err != nil {
			return fmt.Errorf("target: %w", err)
		}
		if len(s.Target.Database) > 50 {
			return errors.New("target: use a database name of at most 50 characters")
		}
	}
	if s.Kind == "copy" && s.Source.ServerID == s.Target.ServerID && s.Source.Container == s.Target.Container && s.Source.Database == s.Target.Database {
		return errors.New("the source and the target are the same database")
	}
	if (s.Kind == "backup" || s.Kind == "restore") && s.File == "" {
		return errors.New("choose a file")
	}
	switch s.IfExists {
	case "", "stop":
		s.IfExists = "stop"
	case "replace", "keep":
	default:
		return errors.New("invalid choice for an existing database")
	}
	switch s.Compression {
	case "":
		s.Compression = "auto"
	case "auto", "zstd", "lz4", "gzip", "none":
	default:
		return errors.New("invalid compression")
	}
	if s.Jobs < 0 || s.Jobs > 32 || s.RestoreJobs < 0 || s.RestoreJobs > 32 {
		return errors.New("parallel jobs must be between 1 and 32")
	}
	for _, p := range s.ExcludeData {
		if p == "" || len(p) > 200 || strings.ContainsAny(p, "\x00\r\n") {
			return errors.New("invalid table pattern")
		}
	}
	if s.Kind != "copy" {
		s.Verify = false
	}
	if s.Kind != "copy" || sameContainer(*s) {
		s.Direct = false
	}
	if s.Direct {
		s.DirectHost = strings.TrimSpace(s.DirectHost)
		if s.DirectHost != "" && !hostRe.MatchString(s.DirectHost) {
			return errors.New("invalid source address")
		}
		if s.DirectPort < 0 || s.DirectPort > 65535 {
			return errors.New("invalid source port")
		}
		if s.File != "" {
			return errors.New("a direct transfer cannot keep a backup file on this computer")
		}
	}
	return nil
}

// Meta describes a backup; it is stored as sd-backup.json inside the backup file.
type Meta struct {
	Format    int     `json:"format"`
	Database  string  `json:"database"`
	Server    string  `json:"server"`
	Container string  `json:"container"`
	Version   int     `json:"version"` // server_version_num of the source
	PgDump    string  `json:"pgDump"`
	Encoding  string  `json:"encoding"`
	Collate   string  `json:"collate"`
	Ctype     string  `json:"ctype"`
	Bytes     int64   `json:"bytes"` // database size on the source
	Created   int64   `json:"created"`
	Schema    bool    `json:"schemaOnly"`
	Tables    []Table `json:"tables"`
}

const metaName = "sd-backup.json"

// ------------------------------------------------------------------ view

// Phase is one step of a job, with its own progress.
type Phase struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	State   string  `json:"state"` // pending, running, done, failed, skipped
	Done    int64   `json:"done"`
	Total   int64   `json:"total"`
	Unit    string  `json:"unit"` // bytes or items
	Detail  string  `json:"detail"`
	Rate    float64 `json:"rate"` // units per second
	ETA     int64   `json:"eta"`  // seconds left, -1 unknown
	Started int64   `json:"started"`
	Ended   int64   `json:"ended"`
	weight  float64
}

// Mismatch is a table whose row count differs after a copy.
type Mismatch struct {
	Table  string `json:"table"`
	Source int64  `json:"source"`
	Target int64  `json:"target"`
}

type Summary struct {
	DatabaseBytes int64      `json:"databaseBytes"`
	BackupBytes   int64      `json:"backupBytes"` // size of the compressed dump
	Tables        int        `json:"tables"`
	Verified      int        `json:"verified"` // tables whose row counts were compared
	Mismatches    []Mismatch `json:"mismatches"`
	SourceVersion int        `json:"sourceVersion"`
	TargetVersion int        `json:"targetVersion"`
	DumpJobs      int        `json:"dumpJobs"`
	RestoreJobs   int        `json:"restoreJobs"`
	Compression   string     `json:"compression"`
	FinalDatabase string     `json:"finalDatabase"`
	OldDatabase   string     `json:"oldDatabase"` // the previous target database, renamed (IfExists keep)
}

// View is what the UI shows of a job.
type View struct {
	ID       string   `json:"id"`
	Spec     Spec     `json:"spec"`
	State    string   `json:"state"` // running, done, warning, failed, cancelled
	Error    string   `json:"error"`
	Phases   []Phase  `json:"phases"`
	Percent  float64  `json:"percent"`
	Active   []Active `json:"active"`
	Done     int      `json:"tablesDone"`
	Total    int      `json:"tablesTotal"`
	Warnings []string `json:"warnings"`
	Log      []string `json:"log"`
	Summary  Summary  `json:"summary"`
	Started  int64    `json:"started"`
	Ended    int64    `json:"ended"`
}

// ------------------------------------------------------------------ job

type Job struct {
	ID       string
	spec     Spec
	src, dst *Side
	changed  func()

	mu       sync.Mutex
	v        View
	tr       *tracker // tables of the running dump or restore
	moved    atomic.Int64
	rates    map[string]*rateMeter
	cancel   context.CancelFunc
	started  bool
	done     chan struct{} // closed when the job has ended
	srcInfo  Instance
	dstInfo  Instance
	meta     Meta
	dir      string // dump folder in the containers
	format   string // "dir" or "custom" (restore of a pg_dump -Fc file)
	created  string // database this job created on the target (dropped when the job fails)
	restored map[string]bool
	addr     Address // direct transfer: where the target reaches the source
	srcUser  string  // direct transfer: the source's login user
}

type rateMeter struct {
	at   time.Time
	done int64
	rate float64
}

// New prepares a job; Start runs it.
func New(id string, spec Spec, src, dst *Side, changed func()) (*Job, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.hasSource() && src == nil || spec.hasTarget() && dst == nil {
		return nil, errors.New("server not found")
	}
	j := &Job{ID: id, spec: spec, src: src, dst: dst, changed: changed, rates: map[string]*rateMeter{}, dir: "/tmp/sd-" + id, format: "dir", done: make(chan struct{})}
	j.v = View{ID: id, Spec: spec, State: "running", Warnings: []string{}, Log: []string{}, Active: []Active{}, Started: time.Now().UnixMilli()}
	add := func(key, label, unit string, w float64) {
		j.v.Phases = append(j.v.Phases, Phase{Key: key, Label: label, State: "pending", Unit: unit, ETA: -1, weight: w})
	}
	add("prepare", "Prepare", "items", 1)
	if spec.Direct {
		add("dump", "Back up (directly to the target)", "bytes", 45)
	} else if spec.hasSource() {
		add("dump", "Back up", "bytes", 35)
	}
	switch {
	case spec.Direct:
	case spec.Kind == "copy":
		add("transfer", "Transfer", "bytes", 20)
	case spec.Kind == "backup":
		add("transfer", "Download", "bytes", 20)
	case spec.Kind == "restore":
		add("transfer", "Upload", "bytes", 20)
	}
	if spec.hasTarget() {
		add("restore", "Restore data", "bytes", 30)
		add("indexes", "Indexes & constraints", "items", 12)
		add("finish", "Finish", "items", 2)
	}
	return j, nil
}

// Queue marks a job that waits for its turn (several databases at once).
func (j *Job) Queue() {
	j.mu.Lock()
	j.v.State = "queued"
	j.mu.Unlock()
}

// Done is closed when the job has ended (or was cancelled before it started).
func (j *Job) Done() <-chan struct{} { return j.done }

// Start runs the job in the background. A job cancelled while queued does not start.
func (j *Job) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	j.mu.Lock()
	if j.v.State == "cancelled" {
		j.mu.Unlock()
		cancel()
		return
	}
	j.cancel, j.started = cancel, true
	j.v.State, j.v.Started = "running", time.Now().UnixMilli()
	j.mu.Unlock()
	j.changed()
	go func() {
		defer close(j.done)
		defer cancel()
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			beat := time.Now()
			for {
				select {
				case <-t.C:
					j.sample()
					j.changed()
					if time.Since(beat) >= heartbeatEvery {
						beat = time.Now()
						go j.heartbeat()
					}
				case <-stop:
					return
				}
			}
		}()
		err := j.run(ctx)
		close(stop)
		j.end(ctx, err)
		j.changed()
	}()
}

// Cancel stops the job; the processes on the servers are killed and the leftovers removed.
func (j *Job) Cancel() {
	j.mu.Lock()
	if !j.started {
		if j.v.State != "cancelled" {
			now := time.Now().UnixMilli()
			j.v.State, j.v.Error, j.v.Started, j.v.Ended = "cancelled", "Cancelled before it started", now, now
			close(j.done)
		}
		j.mu.Unlock()
		j.changed()
		return
	}
	cancel := j.cancel
	j.mu.Unlock()
	cancel()
}

// View returns a copy of the job's state.
func (j *Job) View() View {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := j.v
	v.Phases = append([]Phase(nil), j.v.Phases...)
	v.Warnings = append([]string(nil), j.v.Warnings...)
	v.Log = append([]string(nil), j.v.Log...)
	v.Active = append([]Active(nil), j.v.Active...)
	var sum, w float64
	for _, p := range v.Phases {
		if p.State == "skipped" {
			continue
		}
		w += p.weight
		switch {
		case p.State == "done":
			sum += p.weight
		case p.Total > 0:
			sum += p.weight * min(1, float64(p.Done)/float64(p.Total))
		}
	}
	if w > 0 {
		v.Percent = 100 * sum / w
	}
	if v.State == "done" || v.State == "warning" {
		v.Percent = 100
	}
	return v
}

// ------------------------------------------------------------------ state helpers

func (j *Job) phase(key string) *Phase {
	for i := range j.v.Phases {
		if j.v.Phases[i].Key == key {
			return &j.v.Phases[i]
		}
	}
	return &Phase{} // not part of this job
}

func (j *Job) begin(key string, total int64, detail string) {
	j.mu.Lock()
	p := j.phase(key)
	p.State, p.Total, p.Done, p.Detail, p.Started = "running", total, 0, detail, time.Now().UnixMilli()
	j.mu.Unlock()
	j.changed()
}

func (j *Job) detail(key, text string) {
	j.mu.Lock()
	j.phase(key).Detail = text
	j.mu.Unlock()
	j.changed()
}

func (j *Job) progress(key string, done, total int64) {
	j.mu.Lock()
	p := j.phase(key)
	p.Done = done
	if total >= 0 {
		p.Total = total
	}
	j.mu.Unlock()
}

func (j *Job) finishPhase(key, state string) {
	j.mu.Lock()
	p := j.phase(key)
	if p.State == "running" || p.State == "pending" {
		p.State = state
		if state == "done" {
			p.Done = max(p.Done, p.Total)
		}
		p.Ended = time.Now().UnixMilli()
		p.ETA = -1
	}
	if j.tr != nil && (key == "dump" || key == "restore") {
		j.v.Active = []Active{}
	}
	j.mu.Unlock()
	j.changed()
}

func (j *Job) log(line string) {
	j.mu.Lock()
	j.v.Log = append(j.v.Log, line)
	if len(j.v.Log) > 500 {
		j.v.Log = j.v.Log[len(j.v.Log)-400:]
	}
	j.mu.Unlock()
}

func (j *Job) warn(msg string) {
	j.mu.Lock()
	if len(j.v.Warnings) < 100 {
		j.v.Warnings = append(j.v.Warnings, msg)
	}
	j.mu.Unlock()
	j.log("⚠ " + msg)
}

// sample refreshes the table progress, rates and ETAs (once a second).
func (j *Job) sample() {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	for i := range j.v.Phases {
		p := &j.v.Phases[i]
		if p.State != "running" {
			continue
		}
		switch {
		case p.Key == "transfer":
			p.Done = j.moved.Load()
		case (p.Key == "dump" || p.Key == "restore") && j.tr != nil:
			done, active := j.tr.progress()
			p.Done = done
			j.v.Active = active
			j.v.Done, j.v.Total = j.tr.nDone, len(j.tr.items)
		}
		m := j.rates[p.Key]
		if m == nil {
			j.rates[p.Key] = &rateMeter{at: now, done: p.Done}
			continue
		}
		if dt := now.Sub(m.at).Seconds(); dt >= 1 {
			r := float64(p.Done-m.done) / dt
			if m.rate == 0 {
				m.rate = r
			} else {
				m.rate = 0.7*m.rate + 0.3*r // smooth the bursts
			}
			m.at, m.done = now, p.Done
		}
		p.Rate = m.rate
		p.ETA = -1
		if m.rate > 0 && p.Total > p.Done {
			p.ETA = int64(float64(p.Total-p.Done) / m.rate)
		}
	}
}

// ------------------------------------------------------------------ run

func (j *Job) run(ctx context.Context) error {
	s := j.spec
	j.begin("prepare", 0, "Checking the servers")
	if s.hasSource() {
		in, err := Probe(ctx, *j.src, s.Source.Container)
		if err != nil {
			return fmt.Errorf("source %s: %w", j.src.Server, err)
		}
		db, ok := in.Database(s.Source.Database)
		if !ok {
			return fmt.Errorf("database %s does not exist on %s", s.Source.Database, j.src.Server)
		}
		j.srcInfo = in
		j.meta = Meta{Format: 1, Database: db.Name, Server: j.src.Server, Container: in.Container, Version: in.Version, PgDump: in.PgDump,
			Encoding: db.Encoding, Collate: db.Collate, Ctype: db.Ctype, Bytes: db.Bytes, Created: time.Now().UnixMilli(), Schema: s.SchemaOnly}
		j.mu.Lock()
		j.v.Summary.DatabaseBytes, j.v.Summary.SourceVersion = db.Bytes, in.Version
		j.mu.Unlock()
		j.log(fmt.Sprintf("Source: %s / %s / %s (PostgreSQL %s, %s)", j.src.Server, in.Container, db.Name, VersionText(in.Version), human(db.Bytes)))
	}
	if s.Kind == "restore" {
		info, err := ReadBackupFile(s.File)
		if err != nil {
			return err
		}
		j.format = info.Format
		if info.Meta != nil {
			j.meta = *info.Meta
		}
		j.mu.Lock()
		j.v.Summary.BackupBytes, j.v.Summary.SourceVersion = info.Bytes, j.meta.Version
		j.mu.Unlock()
	}
	if s.hasTarget() {
		in, err := Probe(ctx, *j.dst, s.Target.Container)
		if err != nil {
			return fmt.Errorf("target %s: %w", j.dst.Server, err)
		}
		j.dstInfo = in
		j.mu.Lock()
		j.v.Summary.TargetVersion = in.Version
		j.mu.Unlock()
		if _, exists := in.Database(s.Target.Database); exists && s.IfExists == "stop" {
			return fmt.Errorf("database %s already exists on %s", s.Target.Database, j.dst.Server)
		}
		if j.meta.Version > 0 && in.Major() < j.meta.Version/10000 {
			j.warn(fmt.Sprintf("The target runs PostgreSQL %s, older than the source (%s): some objects may fail to restore.",
				VersionText(in.Version), VersionText(j.meta.Version)))
		}
		if !in.CreateDB && !in.Superuser {
			return fmt.Errorf("the PostgreSQL user %s on %s may not create databases", in.User, j.dst.Server)
		}
		j.log(fmt.Sprintf("Target: %s / %s / %s (PostgreSQL %s)", j.dst.Server, in.Container, s.Target.Database, VersionText(in.Version)))
	}
	j.detail("prepare", "Removing leftovers of interrupted jobs")
	j.housekeep(ctx)
	if s.Direct {
		if err := j.prepareDirect(ctx); err != nil {
			return err
		}
	}
	j.finishPhase("prepare", "done")

	if s.hasSource() {
		if err := j.dump(ctx); err != nil {
			j.finishPhase("dump", "failed")
			return err
		}
		j.finishPhase("dump", "done")
	}
	switch {
	case j.sameContainer():
		j.finishPhase("transfer", "skipped")
		j.log("Source and target are the same container: nothing to transfer.")
	case s.Direct: // the backup is already on the target
	default:
		if err := j.transfer(ctx); err != nil {
			j.finishPhase("transfer", "failed")
			return err
		}
		j.finishPhase("transfer", "done")
		if s.hasSource() {
			j.removeDump(ctx, j.src, s.Source.Container)
		}
	}
	if s.hasTarget() {
		if err := j.restore(ctx); err != nil {
			j.finishPhase("restore", "failed")
			j.finishPhase("indexes", "failed")
			return err
		}
		j.removeDump(ctx, j.dst, s.Target.Container)
		if err := j.finish(ctx); err != nil {
			j.finishPhase("finish", "failed")
			return err
		}
		j.finishPhase("finish", "done")
	}
	return nil
}

func (j *Job) sameContainer() bool { return sameContainer(j.spec) }

// prepareDirect checks that the target may read the source directly and gives it the source's login.
func (j *Job) prepareDirect(ctx context.Context) error {
	c, l := detectDirect(ctx, j.spec, j.src, j.dst, j.srcInfo.Major(), j.dstInfo.Major(), 15)
	if !c.OK {
		return fmt.Errorf("direct transfer: %s (untick the direct transfer to go through this computer)", c.Message)
	}
	if err := writePgpass(ctx, *j.dst, j.spec.Target.Container, j.dir+".pgpass", l); err != nil {
		return err
	}
	addr := *c.Address
	j.addr, j.srcUser = addr, l.user
	j.log(fmt.Sprintf("Direct transfer: %s reads %s at %s; nothing goes through this computer", j.dst.Server, j.spec.Source.Database, addr))
	return nil
}

// end records the outcome and removes what the job left on the servers.
func (j *Job) end(ctx context.Context, err error) {
	cleanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	failed := err != nil
	if failed {
		j.killAll(cleanCtx)
	}
	j.cleanup(cleanCtx, failed)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.v.Ended = time.Now().UnixMilli()
	for i := range j.v.Phases {
		if p := &j.v.Phases[i]; p.State == "running" {
			p.State, p.Ended = "failed", j.v.Ended
		}
	}
	switch {
	case ctx.Err() != nil:
		j.v.State, j.v.Error = "cancelled", "Cancelled"
	case failed:
		j.v.State, j.v.Error = "failed", err.Error()
	case len(j.v.Warnings) > 0:
		j.v.State = "warning"
	default:
		j.v.State = "done"
	}
	j.v.Active = []Active{}
	if j.v.Error != "" {
		j.v.Log = append(j.v.Log, "✘ "+j.v.Error)
	} else {
		j.v.Log = append(j.v.Log, fmt.Sprintf("✔ Finished in %s", (time.Duration(j.v.Ended-j.v.Started)*time.Millisecond).Round(time.Second)))
	}
}

// ------------------------------------------------------------------ processes in the containers

// pgCommand runs a long PostgreSQL tool in a container. Its pid goes to <dir>.pid and its connections are
// named after the job, so a cancelled job can kill it (closing SSH does not stop docker exec).
// stderr is handed to onLine; meanwhile, poll (when set) runs every 2 seconds.
func (j *Job) pgCommand(ctx context.Context, side *Side, container, tool string, onLine func(string), poll func(context.Context)) error {
	script := pgEnv + "export PGAPPNAME=" + quote("sd-"+j.ID) + "\necho $$ > " + j.dir + ".pid\nexec " + tool
	var tail []string
	lw := &lineWriter{fn: func(l string) {
		if l == "" {
			return
		}
		tail = append(tail, l)
		if len(tail) > 30 {
			tail = tail[1:]
		}
		onLine(l)
	}}
	pctx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	if poll != nil {
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-pctx.Done():
					return
				case <-t.C:
					poll(pctx)
				}
			}
		}()
	}
	var out strings.Builder
	err := side.Host.Run(ctx, dexec(side.Docker, container, false, script), nil, &out, lw)
	stopPoll()
	if err != nil {
		var ee *ExitError
		if errors.As(err, &ee) {
			return &toolError{code: ee.Code, tail: tail}
		}
		return err
	}
	return nil
}

type toolError struct {
	code int
	tail []string
}

func (e *toolError) Error() string {
	var errs []string
	for _, l := range e.tail {
		if strings.Contains(l, "error:") || strings.Contains(l, "ERROR") || strings.Contains(l, "FATAL") || strings.Contains(l, "fatal") {
			errs = append(errs, l)
		}
	}
	if len(errs) == 0 {
		errs = e.tail
	}
	if len(errs) > 5 {
		errs = errs[len(errs)-5:]
	}
	if len(errs) == 0 {
		return fmt.Sprintf("exit code %d", e.code)
	}
	return friendly(strings.Join(errs, "\n"))
}

// pollCopies reads pg_stat_progress_copy in db and feeds the tracker.
func (j *Job) pollCopies(side *Side, container, db, command string, version int) func(context.Context) {
	if version < 140000 {
		return nil
	}
	script := pgEnv + "psql -XAtq -F \"$T\" -d " + quote(db) + " <<'SQL'\n" + fmt.Sprintf(progressSQL, command) + "\nSQL\n"
	cmd := dexec(side.Docker, container, false, script)
	return func(ctx context.Context) {
		out, err := run(ctx, side.Host, cmd, 15*time.Second)
		if err != nil {
			return
		}
		active := parseProgress(out)
		j.mu.Lock()
		if j.tr != nil {
			j.tr.poll(active)
		}
		j.mu.Unlock()
	}
}

func (j *Job) killAll(ctx context.Context) {
	kill := func(side *Side, container, maint string) {
		if side == nil || container == "" {
			return
		}
		if maint == "" {
			maint = "postgres"
		}
		script := "[ -f " + j.dir + ".pid ] && kill $(cat " + j.dir + ".pid) 2>/dev/null\n" + pgEnv +
			"psql -XAtq -d " + quote(maint) + " -c " + quote("select pg_terminate_backend(pid) from pg_stat_activity where application_name = "+lit("sd-"+j.ID)) + " >/dev/null 2>&1\ntrue"
		_, _ = run(ctx, side.Host, dexec(side.Docker, container, false, script), 30*time.Second)
	}
	if j.spec.hasSource() {
		kill(j.src, j.spec.Source.Container, j.srcInfo.MaintDB)
	}
	if j.spec.hasTarget() {
		kill(j.dst, j.spec.Target.Container, j.dstInfo.MaintDB)
	}
}

// cleanup removes the dump folders, and the half-restored database of a failed job.
func (j *Job) cleanup(ctx context.Context, failed bool) {
	rm := func(side *Side, container string) {
		_, err := run(ctx, side.Host, dexec(side.Docker, container, false, "rm -rf "+j.dir+" "+j.dir+".*"), time.Minute)
		if err != nil {
			j.warn("Could not remove " + j.dir + " on " + side.Server + ": " + err.Error())
		}
	}
	if j.spec.hasSource() && j.src != nil {
		rm(j.src, j.spec.Source.Container)
	}
	if j.spec.hasTarget() && j.dst != nil && !j.sameContainer() {
		rm(j.dst, j.spec.Target.Container)
	}
	if failed && j.created != "" {
		if err := j.sqlTarget(ctx, dropDatabase(j.created, j.dstInfo.Version)); err != nil {
			j.warn("Could not drop the unfinished database " + j.created + ": " + err.Error())
		} else {
			j.log("Dropped the unfinished database " + j.created)
		}
	}
}

// ------------------------------------------------------------------ dump

func (j *Job) dumpJobs() int {
	n := autoJobs(j.spec.Jobs, j.srcInfo)
	if j.spec.Direct { // pg_dump runs on the target: spare its cores too
		n = min(n, autoJobs(j.spec.Jobs, j.dstInfo))
	}
	return n
}

// dumpSide is where pg_dump runs and writes the backup folder: the source, or the target in a direct transfer.
func (j *Job) dumpSide() (*Side, string) {
	if j.spec.Direct {
		return j.dst, j.spec.Target.Container
	}
	return j.src, j.spec.Source.Container
}

// autoJobs picks the number of parallel jobs. Auto uses half the CPU cores (at most 8), so the server
// keeps cores free for its applications while the backup or restore runs.
func autoJobs(want int, in Instance) int {
	n := want
	if n <= 0 {
		n = min(max(in.Cores/2, 1), 8)
	}
	// pg_dump / pg_restore open one connection per job (+1).
	if in.MaxConn > 0 {
		if free := in.MaxConn - in.Conns - 3; free < n {
			n = max(free, 1)
		}
	}
	return n
}

// compressionArg picks pg_dump's -Z for the requested method and server version.
func compressionArg(method string, major int) (arg, label string) {
	if major >= 16 {
		switch method {
		case "auto", "zstd":
			return "-Z zstd:3", "zstd"
		case "lz4":
			return "-Z lz4", "lz4"
		case "none":
			return "-Z 0", "none"
		}
		return "-Z gzip:3", "gzip"
	}
	if method == "none" {
		return "-Z 0", "none"
	}
	return "-Z 3", "gzip"
}

func (j *Job) dump(ctx context.Context) error {
	s := j.spec
	j.begin("dump", 0, "Reading the table sizes")
	tables, err := listTables(ctx, *j.src, s.Source.Container, s.Source.Database)
	if err != nil {
		return fmt.Errorf("could not list the tables: %w", err)
	}
	j.meta.Tables = tables
	tr := newTracker()
	if !s.SchemaOnly {
		for _, t := range tables {
			tr.add(t.Key(), t.Bytes, t.Bytes)
		}
	}
	jobs := j.dumpJobs()
	method := s.Compression
	for attempt := 0; ; attempt++ {
		j.mu.Lock()
		j.tr = tr
		j.v.Total = len(tr.items)
		j.mu.Unlock()
		runner, runCont := j.dumpSide()
		major := j.srcInfo.Major()
		if s.Direct {
			major = j.dstInfo.Major()
		}
		arg, label := compressionArg(method, major)
		j.mu.Lock()
		j.v.Summary.DumpJobs, j.v.Summary.Compression, j.v.Summary.Tables = jobs, label, len(tables)
		j.mu.Unlock()
		j.progress("dump", 0, tr.total)
		where := ""
		if s.Direct {
			where = fmt.Sprintf("%s reads %s · ", j.dst.Server, j.addr)
		}
		j.detail("dump", fmt.Sprintf("%sReading the schema · %d parallel jobs · %s", where, jobs, label))
		j.log(fmt.Sprintf("pg_dump: %d tables, %s of table data, %d parallel jobs, %s compression", len(tables), human(tr.total), jobs, label))
		tool := fmt.Sprintf("pg_dump -Fd -j %d %s -v -f %s", jobs, arg, j.dir)
		if s.SchemaOnly {
			tool += " --schema-only"
		}
		for _, p := range s.ExcludeData {
			tool += " --exclude-table-data=" + quote(p)
		}
		tool += " -d " + quote(s.Source.Database)
		if s.Direct {
			tool = directEnv(j.addr, j.srcUser, j.dir+".pgpass", 15) + tool
		}
		// pg_dump -Fd wants a folder that does not exist yet.
		if _, err := run(ctx, runner.Host, dexec(runner.Docker, runCont, false, "rm -rf "+j.dir), time.Minute); err != nil {
			return err
		}
		dataStarted := false
		err = j.pgCommand(ctx, runner, runCont, tool, func(l string) {
			j.log(l)
			if m := dumpTableRe.FindStringSubmatch(l); m != nil {
				j.mu.Lock()
				tr.start(m[1])
				j.mu.Unlock()
				if !dataStarted {
					dataStarted = true
					j.detail("dump", fmt.Sprintf("%sCopying table data · %d parallel jobs · %s", where, jobs, label))
				}
			} else if m := finishedRe.FindStringSubmatch(l); m != nil && strings.HasPrefix(m[2], "TABLE DATA ") {
				j.mu.Lock()
				tr.finishName(strings.TrimPrefix(m[2], "TABLE DATA "))
				j.mu.Unlock()
			}
		}, j.pollCopies(j.src, s.Source.Container, s.Source.Database, "COPY TO", j.srcInfo.Version))
		var te *toolError
		if err != nil && errors.As(err, &te) && attempt == 0 && method != "gzip" && strings.Contains(strings.Join(te.tail, "\n"), "not support") {
			j.warn("This pg_dump was built without " + label + ": using gzip instead.")
			method = "gzip"
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("pg_dump failed: %w", err)
		}
		break
	}
	j.mu.Lock()
	tr.finishAll()
	j.mu.Unlock()
	j.sample()
	// The description of the backup travels inside it.
	meta, _ := json.Marshal(j.meta)
	runner, runCont := j.dumpSide()
	if err := runner.Host.Run(ctx, dexec(runner.Docker, runCont, true, "cat > "+j.dir+"/"+metaName), strings.NewReader(string(meta)), io.Discard, io.Discard); err != nil {
		return fmt.Errorf("could not write the backup description: %w", err)
	}
	files, err := j.listDump(ctx, runner, runCont)
	if err != nil {
		return err
	}
	var total int64
	for _, size := range files {
		total += size
	}
	j.mu.Lock()
	j.v.Summary.BackupBytes = total
	j.mu.Unlock()
	j.log(fmt.Sprintf("Backup size: %s (%d files)", human(total), len(files)))
	return nil
}

func (j *Job) listDump(ctx context.Context, side *Side, container string) (map[string]int64, error) {
	out, err := run(ctx, side.Host, dexec(side.Docker, container, false, "cd "+j.dir+" && ls -ln"), time.Minute)
	if err != nil {
		return nil, fmt.Errorf("could not read the backup folder: %w", err)
	}
	return parseLs(out), nil
}

// ------------------------------------------------------------------ transfer

// tarOut writes the dump folder as a tar stream, with the description first so it can be read quickly.
const tarOut = `cd %s && exec tar -cf - ` + metaName + ` toc.dat $(ls -A | grep -v -x -e ` + metaName + ` -e toc.dat)`

func (j *Job) transfer(ctx context.Context) error {
	s := j.spec
	j.moved.Store(0)
	var total int64
	switch s.Kind {
	case "copy", "backup":
		total = j.v.Summary.BackupBytes + 1024
	case "restore":
		st, err := os.Stat(s.File)
		if err != nil {
			return err
		}
		total = st.Size()
	}
	if s.hasTarget() && j.dstInfo.TmpFreeKB > 0 && j.dstInfo.TmpFreeKB*1024 < total+total/20 {
		return fmt.Errorf("not enough free space in /tmp of %s on %s: %s needed, %s free",
			s.Target.Container, j.dst.Server, human(total), human(j.dstInfo.TmpFreeKB*1024))
	}
	var label string
	switch s.Kind {
	case "copy":
		label = "From " + j.src.Server + " to " + j.dst.Server + " through this computer"
	case "backup":
		label = "From " + j.src.Server + " to this computer"
	case "restore":
		label = "From this computer to " + j.dst.Server
	}
	j.begin("transfer", total, label)

	var local *os.File
	part := ""
	if s.File != "" && s.Kind != "restore" {
		part = s.File + ".part"
		f, err := os.Create(part)
		if err != nil {
			return err
		}
		local = f
		defer func() {
			if local != nil {
				local.Close()
				os.Remove(part)
			}
		}()
	}
	counter := writerFunc(func(p []byte) (int, error) { j.moved.Add(int64(len(p))); return len(p), nil })

	var err error
	switch s.Kind {
	case "backup":
		err = j.src.Host.Run(ctx, dexec(j.src.Docker, s.Source.Container, false, fmt.Sprintf(tarOut, j.dir)), nil, io.MultiWriter(local, counter), &tailBuf{})
	case "restore":
		var f *os.File
		f, err = os.Open(s.File)
		if err != nil {
			return err
		}
		defer f.Close()
		dest := "rm -rf " + j.dir + " && mkdir -p " + j.dir + " && exec tar -xf - -C " + j.dir
		if j.format == "custom" {
			dest = "rm -rf " + j.dir + " && mkdir -p " + j.dir + " && exec cat > " + j.dir + "/backup.dump"
		}
		stderr := &tailBuf{}
		err = j.dst.Host.Run(ctx, dexec(j.dst.Docker, s.Target.Container, true, dest), io.TeeReader(f, counter), io.Discard, stderr)
		if err != nil && stderr.String() != "" {
			err = errors.New(friendly(stderr.String()))
		}
	case "copy":
		err = j.relay(ctx, local, counter)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("transfer failed: %w", err)
	}
	if local != nil {
		if err := local.Close(); err != nil {
			local = nil
			os.Remove(part)
			return err
		}
		local = nil
		os.Remove(s.File)
		if err := os.Rename(part, s.File); err != nil {
			return err
		}
		j.log("Saved " + s.File)
	}
	j.progress("transfer", j.moved.Load(), j.moved.Load())
	return nil
}

// relay streams the dump from the source container to the target container (and to a local copy).
func (j *Job) relay(ctx context.Context, local *os.File, counter io.Writer) error {
	s := j.spec
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var srcErr error
	srcStderr := &tailBuf{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var out io.Writer = io.MultiWriter(pw, counter)
		if local != nil {
			out = io.MultiWriter(pw, local, counter)
		}
		srcErr = j.src.Host.Run(ctx, dexec(j.src.Docker, s.Source.Container, false, fmt.Sprintf(tarOut, j.dir)), nil, out, srcStderr)
		if srcErr != nil {
			pw.CloseWithError(srcErr)
			cancel()
		} else {
			pw.Close()
		}
	}()
	dstStderr := &tailBuf{}
	dstErr := j.dst.Host.Run(ctx, dexec(j.dst.Docker, s.Target.Container, true, "rm -rf "+j.dir+" && mkdir -p "+j.dir+" && exec tar -xf - -C "+j.dir), pr, io.Discard, dstStderr)
	if dstErr != nil {
		pr.CloseWithError(dstErr)
		cancel()
	}
	<-done
	switch {
	case srcErr != nil && srcStderr.String() != "":
		return fmt.Errorf("%s: %s", j.src.Server, friendly(srcStderr.String()))
	case dstErr != nil && dstStderr.String() != "":
		return fmt.Errorf("%s: %s", j.dst.Server, friendly(dstStderr.String()))
	case srcErr != nil:
		return srcErr
	}
	return dstErr
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// tailBuf keeps the last 4 KB written to it (stderr of a stream).
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// ------------------------------------------------------------------ restore

// dumpPath is what pg_restore reads on the target.
func (j *Job) dumpPath() string {
	if j.format == "custom" {
		return j.dir + "/backup.dump"
	}
	return j.dir
}

// workDB is the database the restore writes into. It gets its final name only once complete, so a
// half-restored database never carries the real name, and one left by a killed app is recognised and dropped.
func (j *Job) workDB() string { return j.spec.Target.Database + "_sdnew_" + j.ID[:6] }

func (j *Job) targetExisted() bool {
	_, exists := j.dstInfo.Database(j.spec.Target.Database)
	return exists
}

func (j *Job) restore(ctx context.Context) error {
	s := j.spec
	cont := s.Target.Container
	j.begin("restore", 0, "Reading the backup contents")
	out, err := run(ctx, j.dst.Host, dexec(j.dst.Docker, cont, false, pgEnv+"pg_restore -l "+j.dumpPath()), 2*time.Minute)
	if err != nil {
		return fmt.Errorf("could not read the backup: %w", err)
	}
	toc := parseTOC(out)
	sizes := map[int]int64{}
	if j.format == "dir" {
		files, err := j.listDump(ctx, j.dst, cont)
		if err != nil {
			return err
		}
		sizes = dataFileSizes(files)
	}
	srcSize := map[string]int64{}
	for _, t := range j.meta.Tables {
		srcSize[t.Key()] = t.Bytes
	}
	tr := newTracker()
	byID := map[int]string{}
	posts := map[int]bool{}
	j.restored = map[string]bool{}
	for _, e := range toc {
		switch e.Kind {
		case "data":
			w := sizes[e.ID]
			if w == 0 {
				w = srcSize[e.Key]
			}
			tr.add(e.Key, w, srcSize[e.Key])
			byID[e.ID] = e.Key
			j.restored[e.Key] = true
		case "post":
			posts[e.ID] = true
		}
	}
	// A running table's COPY progress is measured against its size on the source, then scaled to its file.
	for _, it := range tr.items {
		if src := srcSize[it.key]; src > 0 {
			it.ref = src
		} else {
			it.ref = 0
		}
	}
	j.mu.Lock()
	j.tr = tr
	j.v.Done, j.v.Total = 0, len(tr.items)
	if j.v.Summary.Tables == 0 {
		j.v.Summary.Tables = len(tr.items)
	}
	j.mu.Unlock()
	j.progress("restore", 0, tr.total)

	db := j.workDB()
	if err := j.createDatabase(ctx, db); err != nil {
		return err
	}
	jobs := autoJobs(s.restoreWant(), j.dstInfo)
	j.mu.Lock()
	j.v.Summary.RestoreJobs = jobs
	j.mu.Unlock()
	// Index builds get more memory; each parallel job may use it at once.
	mwm := int64(256)
	if j.dstInfo.MemKB > 0 {
		mwm = min(max(j.dstInfo.MemKB/1024/int64(4*jobs), 64), 1024)
	}
	tool := fmt.Sprintf("pg_restore -j %d -v --no-tablespaces", jobs)
	if !s.KeepOwners {
		tool += " --no-owner --no-privileges"
	}
	tool += " -d " + quote(db) + " " + j.dumpPath()
	tool = fmt.Sprintf("env PGOPTIONS='-c maintenance_work_mem=%dMB -c synchronous_commit=off' ", mwm) + tool
	j.detail("restore", fmt.Sprintf("Creating the schema · %d parallel jobs", jobs))
	j.begin("indexes", int64(len(posts)), "Waiting for the data")
	j.log(fmt.Sprintf("pg_restore into %s: %d tables, %d indexes/constraints, %d parallel jobs, maintenance_work_mem %d MB", db, len(tr.items), len(posts), jobs, mwm))
	var postDone int64
	ignored := 0
	var errLines []string
	dataStarted, postStarted := false, false
	lastSerial := ""
	err = j.pgCommand(ctx, j.dst, cont, tool, func(l string) {
		j.log(l)
		j.mu.Lock()
		defer j.mu.Unlock()
		switch {
		case finishedRe.MatchString(l):
			m := finishedRe.FindStringSubmatch(l)
			id, _ := strconv.Atoi(m[1])
			if key, ok := byID[id]; ok {
				tr.finish(key)
			} else if posts[id] {
				postDone++
				j.phase("indexes").Done = postDone
			}
		case launchRe.MatchString(l):
			m := launchRe.FindStringSubmatch(l)
			id, _ := strconv.Atoi(m[1])
			if key, ok := byID[id]; ok {
				tr.start(key)
				if !dataStarted {
					dataStarted = true
					j.phase("restore").Detail = fmt.Sprintf("Loading table data · %d parallel jobs", jobs)
				}
			} else if posts[id] && !postStarted {
				postStarted = true
				j.phase("indexes").Detail = fmt.Sprintf("Building · %d parallel jobs", jobs)
			}
		case restoreDataRe.MatchString(l): // single job: one table after the other
			key := restoreDataRe.FindStringSubmatch(l)[1]
			if lastSerial != "" {
				tr.finish(lastSerial)
			}
			tr.start(key)
			lastSerial = key
		case creatingRe.MatchString(l):
			if lastSerial != "" {
				tr.finish(lastSerial)
				lastSerial = ""
			}
			postDone++
			j.phase("indexes").Done = postDone
		case ignoredRe.MatchString(l):
			ignored, _ = strconv.Atoi(ignoredRe.FindStringSubmatch(l)[1])
		case strings.Contains(l, "error:") || strings.HasPrefix(l, "Command was:"):
			if len(errLines) < 60 {
				errLines = append(errLines, strings.TrimPrefix(l, "pg_restore: "))
			}
		}
	}, j.pollCopies(j.dst, cont, db, "COPY FROM", j.dstInfo.Version))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var te *toolError
		if !(errors.As(err, &te) && ignored > 0) {
			return fmt.Errorf("pg_restore failed: %w", err)
		}
		// pg_restore went on after some statements failed (usually missing roles or extensions).
		j.warn(fmt.Sprintf("%d statement(s) failed during the restore; the rest was restored. See the details below.", ignored))
		for _, l := range errLines {
			if len(l) > 400 {
				l = l[:400] + "…"
			}
			j.warn(l)
		}
	}
	j.mu.Lock()
	tr.finishAll()
	j.mu.Unlock()
	j.sample()
	j.finishPhase("restore", "done")
	j.finishPhase("indexes", "done")
	return nil
}

// createDatabase makes the database to restore into, with the source's encoding and locale when possible.
func (j *Job) createDatabase(ctx context.Context, db string) error {
	base := "CREATE DATABASE " + ident(db) + " TEMPLATE template0"
	var tries []string
	if j.meta.Encoding != "" {
		enc := base + " ENCODING " + lit(j.meta.Encoding)
		if j.meta.Collate != "" && j.meta.Ctype != "" {
			tries = append(tries, enc+" LC_COLLATE "+lit(j.meta.Collate)+" LC_CTYPE "+lit(j.meta.Ctype))
		}
		tries = append(tries, enc)
	}
	tries = append(tries, base)
	var err error
	for i, sql := range tries {
		if err = j.sqlTarget(ctx, sql); err == nil {
			if i > 0 {
				j.warn("The target has no locale " + j.meta.Collate + ": the database was created with the server's default locale.")
			}
			j.created = db
			j.log("Created database " + db)
			return nil
		}
	}
	return fmt.Errorf("could not create database %s: %w", db, err)
}

// sqlTarget runs statements on the target's maintenance database, one by one, stopping at the first error.
func (j *Job) sqlTarget(ctx context.Context, sql string) error {
	maint := j.dstInfo.MaintDB
	if maint == "" {
		maint = "postgres"
	}
	script := pgEnv + "psql -XAtq -v ON_ERROR_STOP=1 -d " + quote(maint) + " <<'SQL'\n" + sql + "\nSQL\n"
	_, err := run(ctx, j.dst.Host, dexec(j.dst.Docker, j.spec.Target.Container, false, script), 10*time.Minute)
	return err
}

func dropDatabase(db string, version int) string {
	if version >= 130000 {
		return "DROP DATABASE IF EXISTS " + ident(db) + " WITH (FORCE);"
	}
	return "select pg_terminate_backend(pid) from pg_stat_activity where datname = " + lit(db) + ";\nDROP DATABASE IF EXISTS " + ident(db) + ";"
}

// ------------------------------------------------------------------ finish

func (j *Job) finish(ctx context.Context) error {
	s := j.spec
	steps := int64(1)
	if s.Analyze {
		steps++
	}
	if s.Verify {
		steps++
	}
	j.begin("finish", steps, "")
	db := j.workDB()
	var step int64
	if s.Analyze && !s.SchemaOnly {
		j.detail("finish", "Updating planner statistics (ANALYZE)")
		jobs := autoJobs(s.restoreWant(), j.dstInfo)
		tool := fmt.Sprintf("vacuumdb --analyze-only -j %d -d %s", jobs, quote(db))
		if err := j.pgCommand(ctx, j.dst, s.Target.Container, tool, j.log, nil); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			j.warn("ANALYZE failed: " + err.Error())
		}
		step++
		j.progress("finish", step, -1)
	}
	final := s.Target.Database
	if !j.targetExisted() {
		j.detail("finish", "Naming the database "+final)
		if err := j.sqlTarget(ctx, "ALTER DATABASE "+ident(db)+" RENAME TO "+ident(final)+";"); err != nil {
			return fmt.Errorf("the new database is complete as %s, but renaming it to %s failed: %w", db, final, err)
		}
	} else {
		// Swap the new database in only now that it is complete.
		old := s.Target.Database
		var sql string
		if s.IfExists == "keep" {
			kept := trimName(old, 40) + "_old_" + time.Now().Format("20060102_1504")
			sql = terminate(old) + "ALTER DATABASE " + ident(old) + " RENAME TO " + ident(kept) + ";\n"
			j.mu.Lock()
			j.v.Summary.OldDatabase = kept
			j.mu.Unlock()
		} else {
			sql = dropDatabase(old, j.dstInfo.Version) + "\n"
		}
		sql += "ALTER DATABASE " + ident(db) + " RENAME TO " + ident(old) + ";"
		j.detail("finish", "Replacing "+old)
		if err := j.sqlTarget(ctx, sql); err != nil {
			return fmt.Errorf("the new database is complete as %s, but replacing %s failed: %w", db, old, err)
		}
		j.log(map[string]string{"keep": "Renamed the old database to " + j.v.Summary.OldDatabase + "; ", "replace": "Dropped the old database; "}[s.IfExists] + db + " is now " + old)
	}
	j.created = "" // complete: a later failure must not drop it
	j.mu.Lock()
	j.v.Summary.FinalDatabase = final
	j.mu.Unlock()
	step++
	j.progress("finish", step, -1)
	if s.Verify && !s.SchemaOnly {
		j.detail("finish", "Comparing row counts")
		if err := j.verify(ctx, final); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			j.warn("Row count check failed: " + err.Error())
		}
	}
	return nil
}

func terminate(db string) string {
	return "select pg_terminate_backend(pid) from pg_stat_activity where datname = " + lit(db) + " and pid <> pg_backend_pid();\n"
}

func trimName(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

const countSQL = `select n.nspname, c.relname,
  (xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', n.nspname, c.relname), false, true, '')))[1]::text
from pg_class c join pg_namespace n on n.oid = c.relnamespace
where c.relkind = 'r' and c.relpersistence <> 't'
  and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg_toast%' and n.nspname not like 'pg_temp%';`

func countRows(ctx context.Context, side *Side, container, db string) (map[string]int64, error) {
	script := pgEnv + "psql -XAtq -v ON_ERROR_STOP=1 -F \"$T\" -d " + quote(db) + " <<'SQL'\n" + countSQL + "\nSQL\n"
	out, err := run(ctx, side.Host, dexec(side.Docker, container, false, script), 6*time.Hour)
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) == 3 {
			n, _ := strconv.ParseInt(f[2], 10, 64)
			counts[f[0]+"."+f[1]] = n
		}
	}
	return counts, nil
}

// verify compares exact row counts of the tables whose data was copied. Rows written to the source after
// the backup started show up as differences.
func (j *Job) verify(ctx context.Context, final string) error {
	var src, dst map[string]int64
	var srcErr, dstErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		src, srcErr = countRows(ctx, j.src, j.spec.Source.Container, j.spec.Source.Database)
	}()
	go func() { defer wg.Done(); dst, dstErr = countRows(ctx, j.dst, j.spec.Target.Container, final) }()
	wg.Wait()
	if srcErr != nil {
		return srcErr
	}
	if dstErr != nil {
		return dstErr
	}
	var miss []Mismatch
	n := 0
	for key := range j.restored {
		a, okA := src[key]
		b, okB := dst[key]
		if !okA || !okB {
			continue
		}
		n++
		if a != b {
			miss = append(miss, Mismatch{Table: key, Source: a, Target: b})
		}
	}
	sort.Slice(miss, func(a, b int) bool { return miss[a].Table < miss[b].Table })
	j.mu.Lock()
	j.v.Summary.Verified, j.v.Summary.Mismatches = n, miss
	j.mu.Unlock()
	if len(miss) > 0 {
		j.warn(fmt.Sprintf("%d of %d tables have a different row count (rows written to the source during the copy also count).", len(miss), n))
	} else {
		j.log(fmt.Sprintf("Row counts match for all %d tables", n))
	}
	return nil
}

// ------------------------------------------------------------------ misc

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// DefaultFileName suggests a name for a backup file.
func DefaultFileName(server, db string, t time.Time) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
				return r
			}
			return '_'
		}, s)
	}
	return fmt.Sprintf("%s_%s_%s.pgbackup.tar", clean(db), clean(server), t.Format("20060102-1504"))
}
