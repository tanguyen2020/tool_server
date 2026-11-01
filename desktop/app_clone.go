package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/crypto/ssh"

	"serverdash/internal/clone"
	"serverdash/internal/monitor"
	"serverdash/internal/store"
)

// ------------------------------------------------------------------ clone containers to another server

// cloneJobs are the clones started in this session, newest first (kept in memory only).
type cloneJobs struct {
	mu   sync.Mutex
	list []*cloneJob
}

type cloneJob struct {
	*clone.Job
	id, dir, targetID string
	cancel            context.CancelFunc // stops the image phase (the app's part)
	scriptStarted     atomic.Bool        // from then on a cancel goes to the script (a "cancel" file)
}

const cloneJobsMax = 20

// CloneRead reads the settings of containers on the source server.
func (a *App) CloneRead(sourceID string, names []string) (clone.Source, error) {
	srv, conn, err := a.server(sourceID)
	if err != nil {
		return clone.Source{}, err
	}
	return clone.Read(a.ctx, conn, srv, names)
}

// CloneReadFile reads a .env file on the source server by its path, and names the containers it belongs to.
func (a *App) CloneReadFile(sourceID, path string, specs []clone.Spec) (clone.File, error) {
	_, conn, err := a.server(sourceID)
	if err != nil {
		return clone.File{}, err
	}
	return clone.ReadEnvFile(a.ctx, conn, path, specs)
}

// ClonePreflight checks the target server before the containers are created there.
func (a *App) ClonePreflight(targetID string, specs []clone.Spec, files []string) (clone.Preflight, error) {
	srv, conn, err := a.server(targetID)
	if err != nil {
		return clone.Preflight{}, err
	}
	return clone.Check(a.ctx, conn, srv, specs, files)
}

// ClonePreview returns the script Start would run (the variables themselves stay in the env files).
func (a *App) ClonePreview(targetID string, plan clone.Plan) (string, error) {
	srv, ok := a.st.Get(targetID)
	if !ok {
		return "", errors.New("server not found")
	}
	return clone.Script(srv, plan, "event")
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// CloneStart writes run.sh and the env files to a private folder on the target (~/.serverdash/clone-…), starts
// it there in the background (it goes on if the connection drops) and follows its output as a job: the UI gets
// "clone.job" events with the progress. Returns the job ID.
func (a *App) CloneStart(targetID string, plan clone.Plan) (string, error) {
	srv, conn, err := a.server(targetID)
	if err != nil {
		return "", err
	}
	tag := randomHex(6)
	script, err := clone.Script(srv, plan, tag)
	if err != nil {
		return "", err
	}
	c, err := conn.SFTP()
	if err != nil {
		return "", err
	}
	home, err := c.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot find the home folder on %s: %w", srv.Name, err)
	}
	base := path.Join(home, ".serverdash")
	if err := c.MkdirAll(base); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", base, err)
	}
	_ = c.Chmod(base, 0o700)
	// Folders of clones that were prepared but never run, or whose log was not collected: remove them after a day.
	if old, err := c.ReadDir(base); err == nil {
		for _, fi := range old {
			if strings.HasPrefix(fi.Name(), "clone-") && fi.IsDir() && time.Since(fi.ModTime()) > 24*time.Hour {
				_ = c.RemoveAll(path.Join(base, fi.Name()))
			}
		}
	}
	name := "clone-" + randomHex(8)
	dir := path.Join(base, name)
	if err := c.Mkdir(dir); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dir, err)
	}
	if err := c.Chmod(dir, 0o700); err != nil {
		_ = c.RemoveAll(dir)
		return "", fmt.Errorf("cannot make %s private: %w", dir, err)
	}
	write := func(file, content string) error {
		f, err := c.OpenFile(path.Join(dir, file), os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return err
		}
		_ = f.Chmod(0o600)
		_, err = f.Write([]byte(content))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	err = write("run.sh", script)
	for i, s := range plan.Specs {
		if env := clone.EnvFile(s); err == nil && env != "" {
			err = write(clone.EnvFileName(i), env)
		}
	}
	for i, f := range plan.Files {
		if err == nil {
			err = write(clone.DataFileName(i), f.Content)
		}
	}
	if err != nil {
		_ = c.RemoveAll(dir)
		return "", fmt.Errorf("cannot write the files to %s: %w", srv.Name, err)
	}

	id := randomHex(8)
	var dirty sync.Mutex
	pending := false
	ctx, cancel := context.WithCancel(a.ctx)
	cj := &cloneJob{id: id, dir: name, targetID: targetID, cancel: cancel}
	cj.Job = clone.NewJob(id, plan, targetID, srv.Name, tag, func() {
		dirty.Lock()
		pending = true
		dirty.Unlock()
	})
	a.clones.mu.Lock()
	a.clones.list = append([]*cloneJob{cj}, a.clones.list...)
	if len(a.clones.list) > cloneJobsMax {
		a.clones.list = a.clones.list[:cloneJobsMax]
	}
	a.clones.mu.Unlock()
	runtime.EventsEmit(a.ctx, "clone.job", cj.View())
	a.audit(targetID, "clone containers", fmt.Sprintf("%d from %s", len(plan.Specs), plan.Source), nil)

	// Events at most every 250 ms (pull and copy output is chatty); the timer also moves the bar during the final check.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				dirty.Lock()
				send := pending || cj.View().Phase == "verify"
				pending = false
				dirty.Unlock()
				if send {
					runtime.EventsEmit(a.ctx, "clone.job", cj.View())
				}
			}
		}
	}()
	end := func() {
		close(stop)
		cancel()
		v := cj.View()
		runtime.EventsEmit(a.ctx, "clone.job", v)
		go a.mon.Collect(targetID)
		a.cloneNotify(v)
	}

	go func() {
		// 1. The images, by the app: the target pulls them, or gets them from the source (no registry needed).
		if images := clone.PlanImages(plan); len(images) > 0 {
			cj.SetPhase("images")
			var src *clone.Side
			if plan.Images != clone.ImagesPull && plan.Images != clone.ImagesLocal {
				if s, c, err := a.server(plan.SourceID); err == nil {
					src = &clone.Side{Srv: s, Conn: c}
				}
			}
			clone.GetImages(ctx, cj.Job, images, plan.Images, src, clone.Side{Srv: srv, Conn: conn})
		}
		if ctx.Err() != nil {
			cj.CancelPending()
			_, _ = conn.Exec(a.ctx, fmt.Sprintf(`rm -rf "$HOME/.serverdash/%s"`, name), 15*time.Second)
			end()
			return
		}

		// 2. The script, detached on the target (nohup) with its output in out.log; tail follows the file until it
		// ends, then the exit code is reported and the folder removed.
		cmd := fmt.Sprintf(`D="$HOME/.serverdash/%s"; : > "$D/out.log"; nohup sh "$D/run.sh" >> "$D/out.log" 2>&1 < /dev/null & pid=$!; `+
			`tail -n +1 -f --pid=$pid "$D/out.log" 2>/dev/null; wait $pid; rc=$?; rm -rf "$D"; exit $rc`, name)
		stream, err := conn.Stream(cmd)
		if err != nil {
			_, _ = conn.Exec(a.ctx, fmt.Sprintf(`rm -rf "$HOME/.serverdash/%s"`, name), 15*time.Second)
			cj.Fail("Cannot start the copy on " + srv.Name + ": " + err.Error())
			end()
			return
		}
		cj.scriptStarted.Store(true)
		if ctx.Err() != nil { // cancelled just as the script started: pass it on
			_, _ = conn.Exec(a.ctx, fmt.Sprintf(`touch "$HOME/.serverdash/%s/cancel"`, name), 15*time.Second)
		}
		sc := bufio.NewScanner(stream)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			cj.Line(sc.Text())
		}
		// A non-zero exit is the script's own result (failed containers), already in the events.
		werr := sc.Err()
		var exit *ssh.ExitError
		if err := stream.Wait(); werr == nil && err != nil && !errors.As(err, &exit) {
			werr = err
		}
		cj.Finish(werr)
		end()
	}()
	return id, nil
}

func (a *App) cloneNotify(v clone.View) {
	ok, failed := 0, 0
	for _, c := range v.Containers {
		if c.Status == "ok" && (c.State == "" || c.State == "running") {
			ok++
		} else if c.Status == "failed" || c.Status == "ok" {
			failed++
		}
	}
	body := fmt.Sprintf("%s → %s: %d of %d running", v.Source, v.Target, ok, len(v.Containers))
	switch v.State {
	case "done":
		a.notify(monitor.Alert{Title: "Containers cloned", Body: body})
	case "warning":
		a.notify(monitor.Alert{Title: fmt.Sprintf("Clone finished: %d need attention", failed), Body: body})
	case "failed":
		a.notify(monitor.Alert{Title: "Clone failed", Body: body + cond(v.Error != "", "\n"+v.Error, ""), Critical: true})
	}
}

func cond(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

func (a *App) cloneJob(id string) *cloneJob {
	a.clones.mu.Lock()
	defer a.clones.mu.Unlock()
	i := slices.IndexFunc(a.clones.list, func(j *cloneJob) bool { return j.id == id })
	if i < 0 {
		return nil
	}
	return a.clones.list[i]
}

// CloneJobs lists this session's clones, newest first.
func (a *App) CloneJobs() []clone.View {
	a.clones.mu.Lock()
	list := slices.Clone(a.clones.list)
	a.clones.mu.Unlock()
	out := make([]clone.View, len(list))
	for i, j := range list {
		out[i] = j.View()
	}
	return out
}

// CloneCancel asks the script to stop before its next container (the one in progress finishes).
func (a *App) CloneCancel(id string) error {
	j := a.cloneJob(id)
	if j == nil {
		return errors.New("clone not found")
	}
	if !j.scriptStarted.Load() {
		j.Cancelling()
		j.cancel()
		runtime.EventsEmit(a.ctx, "clone.job", j.View())
		return nil
	}
	_, conn, err := a.server(j.targetID)
	if err != nil {
		return err
	}
	res, err := conn.Exec(a.ctx, fmt.Sprintf(`D="$HOME/.serverdash/%s"; [ -d "$D" ] && touch "$D/cancel"`, j.dir), 15*time.Second)
	if err == nil && res.Code != 0 {
		err = errors.New("the copy has already ended")
	}
	if err == nil {
		j.Cancelling()
		runtime.EventsEmit(a.ctx, "clone.job", j.View())
	}
	return err
}

// CloneLog returns the output of one step of a clone ("c:2", "pull:0"…) or all of it ("all").
func (a *App) CloneLog(id, key string) (string, error) {
	j := a.cloneJob(id)
	if j == nil {
		return "", errors.New("clone not found")
	}
	return j.Log(key), nil
}

// CloneForget removes a finished clone from the list.
func (a *App) CloneForget(id string) {
	a.clones.mu.Lock()
	defer a.clones.mu.Unlock()
	a.clones.list = slices.DeleteFunc(a.clones.list, func(j *cloneJob) bool { return j.id == id && j.View().State != "running" })
}

func (a *App) CloneProfiles() ([]store.CloneProfile, error) { return a.st.CloneProfiles() }

func (a *App) SaveCloneProfile(p store.CloneProfile) (store.CloneProfile, error) {
	return a.st.SaveCloneProfile(p)
}

func (a *App) DeleteCloneProfile(id string) error { return a.st.DeleteCloneProfile(id) }
