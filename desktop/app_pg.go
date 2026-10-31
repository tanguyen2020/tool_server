package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/crypto/ssh"

	"serverdash/internal/monitor"
	"serverdash/internal/pgdb"
	"serverdash/internal/sshpool"
)

// ------------------------------------------------------------------ PostgreSQL backup / restore / copy

// sshHost runs pgdb's commands on a session of the server's SSH connection.
type sshHost struct{ conn *sshpool.Conn }

func (h sshHost) Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	client, err := h.conn.Client()
	if err != nil {
		return err
	}
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("cannot open SSH session: %w", err)
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, stderr
	if err := sess.Start(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		_ = sess.Close()
		return ctx.Err()
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return &pgdb.ExitError{Code: exitErr.ExitStatus()}
	}
	return err
}

type pgJobs struct {
	mu      sync.Mutex
	running map[string]*pgdb.Job
	history []pgdb.View // finished jobs, newest first
	loaded  bool
}

const pgHistoryMax = 50

func (a *App) pgSide(serverID string) (*pgdb.Side, error) {
	srv, conn, err := a.server(serverID)
	if err != nil {
		return nil, err
	}
	return &pgdb.Side{Host: sshHost{conn}, Docker: sshpool.DockerBin(srv), Server: srv.Name, Address: srv.Host}, nil
}

// pgSides resolves the servers of a spec and fills in their names.
func (a *App) pgSides(spec *pgdb.Spec) (src, dst *pgdb.Side, err error) {
	if spec.Kind == "copy" || spec.Kind == "backup" {
		if src, err = a.pgSide(spec.Source.ServerID); err != nil {
			return nil, nil, fmt.Errorf("source: %w", err)
		}
		spec.Source.Server = src.Server
	}
	if spec.Kind == "copy" || spec.Kind == "restore" {
		if dst, err = a.pgSide(spec.Target.ServerID); err != nil {
			return nil, nil, fmt.Errorf("target: %w", err)
		}
		spec.Target.Server = dst.Server
	}
	return src, dst, nil
}

// PgInstances lists the PostgreSQL containers of a server with their databases.
func (a *App) PgInstances(serverID string) ([]pgdb.Instance, error) {
	side, err := a.pgSide(serverID)
	if err != nil {
		return nil, err
	}
	return pgdb.FindInstances(a.ctx, *side)
}

// PgPreflight checks a job without starting it.
func (a *App) PgPreflight(spec pgdb.Spec) (pgdb.Preflight, error) {
	src, dst, err := a.pgSides(&spec)
	if err != nil {
		return pgdb.Preflight{}, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	return pgdb.RunPreflight(ctx, spec, src, dst), nil
}

// PgDetectDirect checks whether the target of a copy can read the source database directly.
func (a *App) PgDetectDirect(spec pgdb.Spec) (pgdb.DirectCheck, error) {
	spec.Kind = "copy"
	src, dst, err := a.pgSides(&spec)
	if err != nil {
		return pgdb.DirectCheck{}, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, time.Minute)
	defer cancel()
	return pgdb.DetectDirect(ctx, spec, src, dst), nil
}

// PgStart starts a job; its progress arrives as "pgjob" events.
func (a *App) PgStart(spec pgdb.Spec) (string, error) {
	ids, err := a.PgStartMany([]pgdb.Spec{spec}, 1)
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// PgStartMany starts several jobs (one per database), at most parallel of them at a time; the others
// wait as "queued". Nothing starts unless every job is valid.
func (a *App) PgStartMany(specs []pgdb.Spec, parallel int) ([]string, error) {
	if len(specs) == 0 || len(specs) > 100 {
		return nil, errors.New("choose between 1 and 100 databases")
	}
	parallel = min(max(parallel, 1), len(specs))
	jobs := make([]*pgdb.Job, 0, len(specs))
	for _, spec := range specs {
		job, err := a.pgNewJob(spec)
		if err != nil {
			if len(specs) > 1 {
				return nil, fmt.Errorf("%s: %w", spec.Source.Database+spec.Target.Database, err)
			}
			return nil, err
		}
		jobs = append(jobs, job)
	}
	a.pgLoad()
	ids := make([]string, len(jobs))
	for i, job := range jobs {
		ids[i] = job.ID
		job.Queue()
		a.pg.mu.Lock()
		a.pg.running[job.ID] = job
		a.pg.mu.Unlock()
		go a.pgWatch(job)
		runtime.EventsEmit(a.ctx, "pgjob", job.View())
	}
	go func() {
		slots := make(chan struct{}, parallel)
		for _, job := range jobs {
			slots <- struct{}{}
			job.Start(a.ctx) // does nothing if it was cancelled while queued
			go func() { <-job.Done(); <-slots }()
		}
	}()
	return ids, nil
}

// pgNewJob prepares a job whose changes are sent as throttled "pgjob" events.
func (a *App) pgNewJob(spec pgdb.Spec) (*pgdb.Job, error) {
	src, dst, err := a.pgSides(&spec)
	if err != nil {
		return nil, err
	}
	id := newToken()
	var emitMu sync.Mutex
	var last time.Time
	var timer *time.Timer
	var job *pgdb.Job
	// At most ~3 events per second; the last change is always sent.
	changed := func() {
		emitMu.Lock()
		defer emitMu.Unlock()
		if timer != nil {
			return
		}
		wait := 300*time.Millisecond - time.Since(last)
		timer = time.AfterFunc(max(wait, 0), func() {
			emitMu.Lock()
			timer, last = nil, time.Now()
			emitMu.Unlock()
			if job != nil {
				runtime.EventsEmit(a.ctx, "pgjob", job.View())
			}
		})
	}
	job, err = pgdb.New(id, spec, src, dst, changed)
	return job, err
}

// pgWatch waits for a job to end, then records it.
func (a *App) pgWatch(job *pgdb.Job) {
	<-job.Done()
	time.Sleep(400 * time.Millisecond) // let the last throttled event out first
	v := job.View()
	a.pg.mu.Lock()
	delete(a.pg.running, v.ID)
	h := v
	if len(h.Log) > 200 {
		h.Log = h.Log[len(h.Log)-200:]
	}
	a.pg.history = append([]pgdb.View{h}, a.pg.history...)
	if len(a.pg.history) > pgHistoryMax {
		a.pg.history = a.pg.history[:pgHistoryMax]
	}
	a.pg.mu.Unlock()
	a.pgSave()
	runtime.EventsEmit(a.ctx, "pgjob", v)

	s := v.Spec
	var what, serverID, target string
	switch s.Kind {
	case "copy":
		what, serverID, target = "database copy", s.Target.ServerID, fmt.Sprintf("%s/%s → %s/%s", s.Source.Server, s.Source.Database, s.Target.Server, s.Target.Database)
	case "backup":
		what, serverID, target = "database backup", s.Source.ServerID, s.Source.Database+" → "+s.File
	case "restore":
		what, serverID, target = "database restore", s.Target.ServerID, s.File+" → "+s.Target.Database
	}
	var err error
	if v.State == "failed" || v.State == "cancelled" {
		err = errors.New(v.Error)
	}
	a.audit(serverID, what, target, err)
	dur := (time.Duration(v.Ended-v.Started) * time.Millisecond).Round(time.Second)
	switch v.State {
	case "done":
		a.notify(monitor.Alert{Title: "Postgres database " + map[string]string{"copy": "copied", "backup": "backed up", "restore": "restored"}[s.Kind], Body: target + " · " + dur.String()})
	case "warning":
		a.notify(monitor.Alert{Title: "Postgres " + s.Kind + " finished with warnings", Body: target})
	case "failed":
		a.notify(monitor.Alert{Title: "Postgres " + s.Kind + " failed", Body: target + "\n" + v.Error, Critical: true})
	}
}

func (a *App) PgCancel(id string) {
	a.pg.mu.Lock()
	job := a.pg.running[id]
	a.pg.mu.Unlock()
	if job != nil {
		job.Cancel()
	}
}

// PgJobs returns the running jobs, then the finished ones (newest first).
func (a *App) PgJobs() []pgdb.View {
	a.pgLoad()
	a.pg.mu.Lock()
	defer a.pg.mu.Unlock()
	list := make([]pgdb.View, 0, len(a.pg.running)+len(a.pg.history))
	for _, j := range a.pg.running {
		list = append(list, j.View())
	}
	sort.Slice(list, func(x, y int) bool { return list[x].Started > list[y].Started })
	return append(list, a.pg.history...)
}

// PgForget removes a finished job from the history.
func (a *App) PgForget(id string) {
	a.pg.mu.Lock()
	for i, v := range a.pg.history {
		if v.ID == id {
			a.pg.history = append(a.pg.history[:i], a.pg.history[i+1:]...)
			break
		}
	}
	a.pg.mu.Unlock()
	a.pgSave()
}

// PgChooseSaveFile asks where to save a backup.
func (a *App) PgChooseSaveFile(serverName, database string) (string, error) {
	return runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Save the backup of " + database,
		DefaultFilename: pgdb.DefaultFileName(serverName, database, time.Now()),
		Filters:         []runtime.FileFilter{{DisplayName: "PostgreSQL backup (*.pgbackup.tar)", Pattern: "*.pgbackup.tar"}},
	})
}

// PgChooseFolder asks for a folder (backups of several databases).
func (a *App) PgChooseFolder(title string) (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title, CanCreateDirectories: true})
}

// PgBackupFileIn names the backup file of a database in a folder.
func (a *App) PgBackupFileIn(folder, serverName, database string) string {
	return filepath.Join(folder, pgdb.DefaultFileName(serverName, database, time.Now()))
}

// PgChooseBackupFile asks for a backup to restore and reads its description.
func (a *App) PgChooseBackupFile() (*pgdb.FileInfo, error) {
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Restore a backup",
		Filters: []runtime.FileFilter{
			{DisplayName: "PostgreSQL backups (*.pgbackup.tar, *.tar, *.dump, *.backup)", Pattern: "*.pgbackup.tar;*.tar;*.dump;*.backup"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
	if err != nil || path == "" {
		return nil, err
	}
	info, err := pgdb.ReadBackupFile(path)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// BackupFile is a chosen backup file, or why it cannot be restored.
type BackupFile struct {
	pgdb.FileInfo
	Error string `json:"error,omitempty"`
}

// PgChooseBackupFiles asks for one or more backups to restore and reads each one's description.
func (a *App) PgChooseBackupFiles() ([]BackupFile, error) {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Restore backups",
		Filters: []runtime.FileFilter{
			{DisplayName: "PostgreSQL backups (*.pgbackup.tar, *.tar, *.dump, *.backup)", Pattern: "*.pgbackup.tar;*.tar;*.dump;*.backup"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
	if err != nil {
		return nil, err
	}
	list := make([]BackupFile, 0, len(paths))
	for _, p := range paths {
		info, err := pgdb.ReadBackupFile(p)
		f := BackupFile{FileInfo: info}
		f.Path = p
		if err != nil {
			f.Error = err.Error()
		}
		list = append(list, f)
	}
	return list, nil
}

// PgShowFile shows a backup file in the file manager.
func (a *App) PgShowFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return errors.New("the file is no longer there")
	}
	switch goruntime.GOOS {
	case "windows":
		return exec.Command("explorer", "/select,", path).Start()
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	default:
		return exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
}

// ------------------------------------------------------------------ history file

func (a *App) pgHistoryPath() string { return filepath.Join(a.st.Dir(), "pgjobs.json") }

func (a *App) pgLoad() {
	a.pg.mu.Lock()
	defer a.pg.mu.Unlock()
	if a.pg.loaded {
		return
	}
	a.pg.loaded = true
	if a.pg.running == nil {
		a.pg.running = map[string]*pgdb.Job{}
	}
	b, err := os.ReadFile(a.pgHistoryPath())
	if err == nil {
		_ = json.Unmarshal(b, &a.pg.history)
	}
}

func (a *App) pgSave() {
	a.pg.mu.Lock()
	b, err := json.MarshalIndent(a.pg.history, "", " ")
	a.pg.mu.Unlock()
	if err != nil {
		return
	}
	tmp := a.pgHistoryPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, a.pgHistoryPath())
	}
}

// pgShutdown cancels the running jobs and gives them a moment to clean up on the servers.
func (a *App) pgShutdown() {
	a.pg.mu.Lock()
	jobs := make([]*pgdb.Job, 0, len(a.pg.running))
	for _, j := range a.pg.running {
		jobs = append(jobs, j)
	}
	a.pg.mu.Unlock()
	for _, j := range jobs {
		j.Cancel()
	}
	deadline := time.After(15 * time.Second)
	for _, j := range jobs {
		select {
		case <-j.Done():
		case <-deadline:
			return
		}
	}
}

// PgRunning tells whether a job is running (the window asks before closing).
func (a *App) PgRunning() int {
	a.pg.mu.Lock()
	defer a.pg.mu.Unlock()
	return len(a.pg.running)
}
