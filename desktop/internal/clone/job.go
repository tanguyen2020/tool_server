package clone

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Item is one step of a job the UI shows a line for: a .env file, a network, an image or a container.
type Item struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pending, working, ok, exists/local (images, networks), skipped, failed, cancelled
	Step   string `json:"step"`   // containers: removing, creating, starting
	Error  string `json:"error"`  // the message that explains a failure
	// Containers, after the check that follows the start.
	Image    string `json:"image,omitempty"`
	State    string `json:"state,omitempty"` // running, restarting, exited…
	ExitCode int    `json:"exitCode"`
	Restarts int    `json:"restarts"`
	// Images: how the target got it (pull, copy, local) and the bytes copied so far.
	Via     string `json:"via,omitempty"`
	Bytes   int64  `json:"bytes"`
	Size    int64  `json:"size"` // of the image on the source (uncompressed), when copied
	Started int64  `json:"started"`
	Ended   int64  `json:"ended"`
}

// View is the state of a clone job, sent to the UI on every change.
type View struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"`
	TargetID   string   `json:"targetId"`
	Target     string   `json:"target"`
	State      string   `json:"state"` // running, done, warning (some failed or not running), failed, cancelled
	Phase      string   `json:"phase"` // files, networks, pull, containers, verify, done
	Phases     []string `json:"phases"`
	Percent    float64  `json:"percent"`
	Cancelling bool     `json:"cancelling"`
	VerifyWait int      `json:"verifyWait"`
	PhaseAt    int64    `json:"phaseAt"` // when the current phase began (unix ms)
	Started    int64    `json:"started"`
	Ended      int64    `json:"ended"`
	Files      []Item   `json:"files"`
	Networks   []Item   `json:"networks"`
	Images     []Item   `json:"images"`
	Containers []Item   `json:"containers"`
	Now        string   `json:"now"` // the last line of output, for a live hint
	Error      string   `json:"error"`
	Plan       Plan     `json:"plan"` // to retry what failed
}

const maxLogLines = 400

// Job follows run.sh's output. Lines arrive through Line; Finish closes it.
type Job struct {
	mu       sync.Mutex
	v        View
	tag      string
	cur      string              // the item the next log lines belong to ("c:3", "pull:0"…)
	logs     map[string][]string // per item, and "all"
	onChange func()
}

func NewJob(id string, p Plan, targetID, target, tag string, onChange func()) *Job {
	now := time.Now().UnixMilli()
	v := View{ID: id, Source: p.Source, TargetID: targetID, Target: target, State: "running", Started: now, PhaseAt: now,
		VerifyWait: verifyWait, Plan: p, Files: []Item{}, Networks: []Item{}, Images: []Item{}, Containers: []Item{}}
	// The plan is kept to retry failed containers; the file contents (secrets) are not needed for that.
	v.Plan.Files = nil
	for _, f := range p.Files {
		v.Files = append(v.Files, Item{Name: f.Path, Status: "pending"})
	}
	for _, n := range PlanNetworks(p) {
		v.Networks = append(v.Networks, Item{Name: n.Name, Status: "pending"})
	}
	for _, img := range PlanImages(p) {
		v.Images = append(v.Images, Item{Name: img.Name, Status: "pending"})
	}
	for _, s := range p.Specs {
		v.Containers = append(v.Containers, Item{Name: s.Name, Image: s.Image, Status: "pending"})
	}
	for _, ph := range []struct {
		name string
		n    int
	}{{"images", len(v.Images)}, {"files", len(v.Files)}, {"networks", len(v.Networks)}, {"containers", 1}, {"verify", 1}} {
		if ph.n > 0 {
			v.Phases = append(v.Phases, ph.name)
		}
	}
	v.Phase = v.Phases[0]
	return &Job{v: v, tag: "@@" + tag + " ", logs: map[string][]string{}, onChange: onChange}
}

func (j *Job) list(kind string) []Item {
	switch kind {
	case "file":
		return j.v.Files
	case "net":
		return j.v.Networks
	case "img":
		return j.v.Images
	case "c", "state":
		return j.v.Containers
	}
	return nil
}

// Line takes one line of output: an event, or a log line of the item in progress.
func (j *Job) Line(line string) {
	line = strings.TrimRight(line, "\r")
	j.mu.Lock()
	defer func() {
		j.mu.Unlock()
		if j.onChange != nil {
			j.onChange()
		}
	}()
	if !strings.HasPrefix(line, j.tag) {
		j.log(line)
		return
	}
	f := strings.Fields(line[len(j.tag):])
	if len(f) == 0 {
		return
	}
	now := time.Now().UnixMilli()
	switch f[0] {
	case "phase":
		if len(f) > 1 {
			j.v.Phase = f[1]
			j.v.PhaseAt = now
		}
		return
	case "cancelled":
		j.v.Cancelling = false
		j.v.State = "cancelled"
		for i := range j.v.Containers {
			if c := &j.v.Containers[i]; c.Status == "pending" {
				c.Status = "cancelled"
			}
		}
		for i := range j.v.Images {
			if c := &j.v.Images[i]; c.Status == "pending" {
				c.Status = "cancelled"
			}
		}
		return
	case "done":
		j.v.Phase = "done"
		return
	}
	if len(f) < 3 {
		return
	}
	items := j.list(f[0])
	i, err := strconv.Atoi(f[1])
	if err != nil || i < 0 || i >= len(items) {
		return
	}
	it := &items[i]
	key := f[0] + ":" + f[1]
	if f[0] == "state" {
		key = "c:" + f[1]
	}
	j.cur = key
	switch {
	case f[0] == "state":
		it.State = f[2]
		if len(f) > 4 {
			it.ExitCode, _ = strconv.Atoi(f[3])
			it.Restarts, _ = strconv.Atoi(f[4])
		}
	case f[2] == "start":
		it.Status, it.Started = "working", now
	case f[2] == "step" && len(f) > 3:
		it.Step = f[3]
	default: // ok, exists, local, started, skipped, failed
		it.Status, it.Ended = f[2], now
		if it.Status == "started" {
			it.Status = "ok"
		}
		if it.Status == "failed" {
			it.Error = j.reason(key)
		}
	}
}

func (j *Job) log(line string) {
	add := func(k string) {
		l := append(j.logs[k], line)
		if len(l) > maxLogLines {
			l = l[len(l)-maxLogLines:]
		}
		j.logs[k] = l
	}
	add("all")
	if j.cur != "" {
		add(j.cur)
	}
	if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "|") {
		j.v.Now = truncate(t, 200)
	}
}

// reason picks the line that explains a failure: Docker's error, else the last line of the step's log.
func (j *Job) reason(key string) string {
	l := j.logs[key]
	for i := len(l) - 1; i >= 0; i-- {
		t := strings.TrimSpace(l[i])
		if strings.Contains(t, "Error") || strings.Contains(t, "error") || strings.Contains(t, "denied") {
			return truncate(t, 400)
		}
	}
	for i := len(l) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(l[i]); t != "" && !strings.HasPrefix(t, "$ ") {
			return truncate(t, 400)
		}
	}
	return "failed (see the log)"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Cancelling marks a cancel request: the script stops before the next container.
func (j *Job) Cancelling() {
	j.mu.Lock()
	if j.v.State == "running" {
		j.v.Cancelling = true
	}
	j.mu.Unlock()
	if j.onChange != nil {
		j.onChange()
	}
}

// Finish ends the job when the output stream closes; err is why it closed early, if it did.
func (j *Job) Finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := &j.v
	v.Ended = time.Now().UnixMilli()
	v.Cancelling = false
	if v.State == "cancelled" {
		return
	}
	if v.Phase != "done" {
		if err == nil {
			err = errors.New("the output stopped before the end")
		}
		v.State = "failed"
		v.Error = "Lost track of the copy (" + err.Error() + "). It may still be running on the server: check its containers."
		for i := range v.Containers {
			if c := &v.Containers[i]; c.Status == "pending" || c.Status == "working" {
				c.Status = "unknown"
			}
		}
		return
	}
	ok, bad := 0, 0
	for _, c := range v.Containers {
		switch {
		case c.Status == "failed" || (c.Status == "ok" && c.State != "" && c.State != "running"):
			bad++
		case c.Status == "ok":
			ok++
		}
	}
	for _, f := range slices2(v.Files, v.Networks) {
		if f.Status == "failed" {
			bad++
		}
	}
	switch {
	case bad == 0:
		v.State = "done"
	case ok == 0:
		v.State = "failed"
	default:
		v.State = "warning"
	}
}

func slices2(a, b []Item) []Item { return append(append([]Item{}, a...), b...) }

// View returns a copy of the state, with the progress computed.
func (j *Job) View() View {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := j.v
	v.Files = append([]Item{}, v.Files...)
	v.Networks = append([]Item{}, v.Networks...)
	v.Images = append([]Item{}, v.Images...)
	v.Containers = append([]Item{}, v.Containers...)
	// Containers weigh the most; pulls are slow too. The check after the start is the last few percent.
	finished := func(l []Item) float64 {
		n := 0.0
		for _, it := range l {
			if it.Status != "pending" && it.Status != "working" {
				n++
			}
		}
		return n
	}
	total := float64(len(v.Files)+len(v.Networks)) + 2*float64(len(v.Images)) + 4*float64(len(v.Containers))
	done := finished(v.Files) + finished(v.Networks) + 2*finished(v.Images) + 4*finished(v.Containers)
	v.Percent = 95 * done / max(total, 1)
	switch {
	case v.Phase == "verify":
		v.Percent = 95 + 5*min(1, float64(time.Now().UnixMilli()-v.PhaseAt)/float64(verifyWait*1000))
	case v.Phase == "done" || v.State != "running":
		v.Percent = 100
	}
	return v
}

// Log returns the output of one item ("c:2", "pull:0", "file:0", "net:1") or of the whole job ("all").
func (j *Job) Log(key string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.Join(j.logs[key], "\n")
}

// ------------------------------------------------------------------ images (done by the app, not the script)

func (j *Job) changed() {
	if j.onChange != nil {
		j.onChange()
	}
}

// SetPhase moves the job to a phase the app runs itself.
func (j *Job) SetPhase(p string) {
	j.mu.Lock()
	j.v.Phase, j.v.PhaseAt = p, time.Now().UnixMilli()
	j.mu.Unlock()
	j.changed()
}

// Image updates the i-th image: status working/ok/local/failed/cancelled, via pull/copy/local, a message for a failure.
func (j *Job) Image(i int, status, via, msg string) {
	j.mu.Lock()
	if i >= 0 && i < len(j.v.Images) {
		it := &j.v.Images[i]
		now := time.Now().UnixMilli()
		if status == "working" && it.Started == 0 {
			it.Started = now
		}
		if status != "working" {
			it.Ended = now
		}
		it.Status, it.Via = status, via
		if status == "failed" {
			it.Error = truncate(msg, 400)
		}
	}
	j.mu.Unlock()
	j.changed()
}

// ImageBytes reports the progress of a copy.
func (j *Job) ImageBytes(i int, n, size int64) {
	j.mu.Lock()
	if i >= 0 && i < len(j.v.Images) {
		j.v.Images[i].Bytes, j.v.Images[i].Size = n, size
	}
	j.mu.Unlock()
	j.changed()
}

// ImageLog adds a line to the i-th image's log.
func (j *Job) ImageLog(i int, line string) {
	j.mu.Lock()
	j.cur = fmt.Sprintf("img:%d", i)
	j.log(line)
	j.mu.Unlock()
	j.changed()
}

// CancelPending marks what has not run as cancelled (a cancel during the images, before the script started).
func (j *Job) CancelPending() {
	j.mu.Lock()
	j.v.State, j.v.Cancelling = "cancelled", false
	for _, l := range [][]Item{j.v.Images, j.v.Files, j.v.Networks, j.v.Containers} {
		for i := range l {
			if l[i].Status == "pending" || l[i].Status == "working" {
				l[i].Status = "cancelled"
			}
		}
	}
	j.v.Ended = time.Now().UnixMilli()
	j.mu.Unlock()
	j.changed()
}

// Fail ends the job before the script ran (the images could not be had, the files not written).
func (j *Job) Fail(msg string) {
	j.mu.Lock()
	j.v.State, j.v.Error, j.v.Ended = "failed", msg, time.Now().UnixMilli()
	for i := range j.v.Containers {
		if c := &j.v.Containers[i]; c.Status == "pending" {
			c.Status = "cancelled"
		}
	}
	j.mu.Unlock()
	j.changed()
}

// ImagesFailed names the images the target could not get.
func (j *Job) ImagesFailed() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for _, it := range j.v.Images {
		if it.Status == "failed" {
			out = append(out, it.Name)
		}
	}
	return out
}
