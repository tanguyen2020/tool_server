package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"serverdash/internal/activity"
	"serverdash/internal/docker"
	"serverdash/internal/history"
	"serverdash/internal/hostinfo"
	"serverdash/internal/monitor"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
	"serverdash/internal/theme"
	"serverdash/internal/updater"
)

// App holds the methods the UI calls through window.go.main.App.*
type App struct {
	ctx    context.Context
	cancel context.CancelFunc
	st     *store.Store
	pool   *sshpool.Pool
	mon    *monitor.Monitor
	hist   *history.Store // nil when the history database could not be opened

	logsMu sync.Mutex
	logs   map[string]func()

	terms terminals
	tun   tunnels
	runs  runs
	act   *activity.Log
	upd   *updater.Updater

	announced string // update version already announced (installs that cannot update themselves)
}

func NewApp(st *store.Store, hist *history.Store) *App {
	pool := sshpool.New(st)
	a := &App{st: st, pool: pool, mon: monitor.New(st, pool), hist: hist, logs: map[string]func(){}, terms: terminals{list: map[string]*terminal{}},
		tun: tunnels{list: map[string]*tunnel{}}, runs: runs{list: map[string]context.CancelFunc{}}, act: activity.Open(st.Dir())}
	// A removed server takes its port forwards with it.
	st.OnChange(func(id string) {
		if _, ok := st.Get(id); !ok {
			a.stopTunnels(id)
		}
	})
	if hist != nil {
		st.OnChange(func(id string) {
			if _, ok := st.Get(id); !ok {
				_ = hist.DeleteServer(id)
			}
		})
	}
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.mon.OnSnapshot = func(s *monitor.Snapshot) {
		if a.hist != nil {
			a.hist.Add(s)
		}
		runtime.EventsEmit(ctx, "snapshot", s)
	}
	a.mon.OnRemoved = func(id string) { runtime.EventsEmit(ctx, "removed", id) }
	a.mon.OnAlert = a.notify
	a.mon.OnEvents = func(serverID string, events []monitor.Event) {
		if a.hist != nil {
			_ = a.hist.AddEvents(serverID, events)
		}
		runtime.EventsEmit(ctx, "container-events", serverID)
	}
	a.st.OnChange(func(string) { runtime.EventsEmit(ctx, "servers", a.st.ListPublic()) })
	if err := runtime.InitializeNotifications(ctx); err != nil {
		runtime.LogWarning(ctx, "notifications: "+err.Error())
	}
	go a.mon.Run(runCtx)
	go a.autoUpdate(runCtx)
	go a.watchDiskForecasts(runCtx)
}

func (a *App) shutdown(context.Context) {
	a.cancel()
	a.logsMu.Lock()
	for _, stop := range a.logs {
		stop()
	}
	a.logsMu.Unlock()
	a.closeAllTerminals()
	a.stopTunnels("")
	a.pool.CloseAll()
	if a.hist != nil {
		_ = a.hist.Close()
	}
	a.installOnExit()
	runtime.CleanupNotifications(a.ctx)
}

func (a *App) notify(al monitor.Alert) {
	level := "info"
	if al.Critical {
		level = "critical"
	}
	runtime.EventsEmit(a.ctx, "alert", map[string]string{"title": al.Title, "body": al.Body, "level": level})
	if !a.st.Settings().Notifications {
		return
	}
	_ = runtime.SendNotification(a.ctx, runtime.NotificationOptions{ID: al.Title, Title: al.Title, Body: al.Body})
}

// ------------------------------------------------------------------ data

type InitData struct {
	Servers   []store.ServerInfo         `json:"servers"`
	Snapshots []*monitor.Snapshot        `json:"snapshots"`
	History   map[string][]monitor.Point `json:"history"`
	Settings  store.Settings             `json:"settings"`
	DataDir   string                     `json:"dataDir"`
}

func (a *App) Init() InitData {
	all := a.mon.All()
	return InitData{Servers: a.st.ListPublic(), Snapshots: all.Snapshots, History: all.History, Settings: a.st.Settings(), DataDir: a.st.Dir()}
}

func (a *App) SetVisible(v bool) { a.mon.SetVisible(v) }

// Events returns the container events of the last `minutes` minutes (oldest first).
func (a *App) Events(serverID string, minutes int) ([]monitor.Event, error) {
	if a.hist == nil {
		return []monitor.Event{}, nil
	}
	if minutes < 1 || minutes > int(history.Retention.Minutes()) {
		return nil, errors.New("invalid time range")
	}
	now := time.Now()
	return a.hist.Events(serverID, now.Add(-time.Duration(minutes)*time.Minute), now)
}

// History returns stored metrics for the last `minutes` minutes (1-minute resolution or coarser).
func (a *App) History(serverID string, minutes int) (*history.Result, error) {
	if a.hist == nil {
		return nil, errors.New("history database is not available")
	}
	if minutes < 1 || minutes > int(history.Retention.Minutes()) {
		return nil, errors.New("invalid time range")
	}
	now := time.Now()
	return a.hist.Query(serverID, now.Add(-time.Duration(minutes)*time.Minute), now)
}

func (a *App) Refresh(id string) { go a.mon.Collect(id) }

func (a *App) SetSettings(s store.Settings) error { return a.st.SetSettings(s) }

// SetUIPref remembers a view preference of the page (see store.Settings.UI).
func (a *App) SetUIPref(key, value string) error { return a.st.SetUIPref(key, value) }

// SetTheme saves the theme (system, light, dark) and matches the native title bar and window background.
func (a *App) SetTheme(mode string) error {
	set := a.st.Settings()
	set.Theme = mode
	if err := a.st.SetSettings(set); err != nil {
		return err
	}
	applyTheme(a.ctx, mode)
	return nil
}

func applyTheme(ctx context.Context, mode string) {
	switch mode {
	case "light":
		runtime.WindowSetLightTheme(ctx)
	case "dark":
		runtime.WindowSetDarkTheme(ctx)
	default:
		runtime.WindowSetSystemDefaultTheme(ctx)
	}
	r, g, b := theme.Background(mode)
	runtime.WindowSetBackgroundColour(ctx, r, g, b, 255)
}

// ------------------------------------------------------------------ servers

func (a *App) SaveServer(in store.Input) (store.ServerInfo, error) {
	srv, err := a.st.Save(in)
	action := "server edit"
	if in.ID == "" {
		action = "server add"
	}
	if err == nil {
		a.audit(srv.ID, action, srv.Username+"@"+srv.Host, nil)
	}
	return store.ToPublic(srv), err
}

func (a *App) DeleteServer(id string) error {
	srv, _ := a.st.Get(id)
	err := a.st.Remove(id)
	if a.act != nil {
		a.act.Add(id, srv.Name, "server remove", srv.Username+"@"+srv.Host, err)
	}
	return err
}

func (a *App) TestServer(in store.Input) (sshpool.TestResult, error) {
	srv, err := a.st.Draft(in)
	if err != nil {
		return sshpool.TestResult{}, err
	}
	return sshpool.Test(a.ctx, srv, a.pool.Via(srv.JumpID))
}

func (a *App) ResetHostKey(id string) {
	a.audit(id, "reset host key", "", nil)
	a.st.SetHostKey(id, "")
	a.pool.Drop(id)
	go a.mon.Collect(id)
}

func (a *App) PickKeyFile() (string, error) {
	home, _ := os.UserHomeDir()
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Select private key",
		DefaultDirectory: home + string(os.PathSeparator) + ".ssh",
	})
}

// ------------------------------------------------------------------ containers

func (a *App) ContainerAction(serverID, container, action string) error {
	srv, ok := a.st.Get(serverID)
	if !ok {
		return errors.New("server not found")
	}
	conn, err := a.pool.Get(serverID)
	if err != nil {
		return err
	}
	if action != "start" {
		a.mon.Expect(serverID, container)
	}
	err = docker.Action(a.ctx, conn, srv, container, action)
	a.audit(serverID, "container "+action, container, err)
	go a.mon.Collect(serverID)
	return err
}

// RebootServer restarts the server. The monitor shows it as rebooting and reports when it is back.
func (a *App) RebootServer(serverID string) error {
	if _, ok := a.st.Get(serverID); !ok {
		return errors.New("server not found")
	}
	conn, err := a.pool.Get(serverID)
	if err != nil {
		return err
	}
	a.mon.ExpectReboot(serverID)
	if err := hostinfo.Reboot(a.ctx, conn); err != nil {
		a.mon.CancelReboot(serverID)
		a.audit(serverID, "reboot", "", err)
		return err
	}
	a.audit(serverID, "reboot", "", nil)
	go a.mon.Collect(serverID)
	return nil
}

// RemoveContainer deletes a container; force stops a running one first. Volumes and image are kept.
func (a *App) RemoveContainer(serverID, container string, force bool) error {
	srv, ok := a.st.Get(serverID)
	if !ok {
		return errors.New("server not found")
	}
	conn, err := a.pool.Get(serverID)
	if err != nil {
		return err
	}
	a.mon.Expect(serverID, container)
	err = docker.Remove(a.ctx, conn, srv, container, force)
	a.audit(serverID, "container remove", container, err)
	go a.mon.Collect(serverID)
	return err
}

func (a *App) InspectContainer(serverID, container string) (json.RawMessage, error) {
	srv, ok := a.st.Get(serverID)
	if !ok {
		return nil, errors.New("server not found")
	}
	conn, err := a.pool.Get(serverID)
	if err != nil {
		return nil, err
	}
	return docker.Inspect(a.ctx, conn, srv, container)
}

// TopProcesses lists the busiest processes (sortBy: "cpu" or "mem"), measured over one second.
func (a *App) TopProcesses(serverID, sortBy string) ([]hostinfo.Process, error) {
	conn, err := a.pool.Get(serverID)
	if err != nil {
		return nil, err
	}
	return hostinfo.TopProcesses(a.ctx, conn, sortBy)
}

// DockerDiskUsage runs `docker system df` (can take a while on busy hosts).
func (a *App) DockerDiskUsage(serverID string) ([]docker.DiskUsage, error) {
	srv, ok := a.st.Get(serverID)
	if !ok {
		return nil, errors.New("server not found")
	}
	conn, err := a.pool.Get(serverID)
	if err != nil {
		return nil, err
	}
	return docker.SystemDF(a.ctx, conn, srv)
}

// CheckMaintenance refreshes the reboot / pending updates status now.
func (a *App) CheckMaintenance(serverID string) error {
	return a.mon.CheckMaintenance(serverID)
}

type LogEvent struct {
	SID  string `json:"sid"`
	Data string `json:"data,omitempty"`
	End  string `json:"reason,omitempty"`
}

func (a *App) StartLogs(sid, serverID, container string, tail int, since string) error {
	srv, ok := a.st.Get(serverID)
	if !ok {
		return errors.New("server not found")
	}
	a.logsMu.Lock()
	if len(a.logs) >= 6 {
		a.logsMu.Unlock()
		return errors.New("too many log streams open")
	}
	a.logs[sid] = func() {}
	a.logsMu.Unlock()

	conn, err := a.pool.Get(serverID)
	var stop func()
	if err == nil {
		stop, err = docker.Logs(conn, srv, container, tail, since,
			func(data string) { runtime.EventsEmit(a.ctx, "logs.data", LogEvent{SID: sid, Data: data}) },
			func(reason string) {
				a.logsMu.Lock()
				delete(a.logs, sid)
				a.logsMu.Unlock()
				runtime.EventsEmit(a.ctx, "logs.end", LogEvent{SID: sid, End: reason})
			})
	}
	a.logsMu.Lock()
	defer a.logsMu.Unlock()
	if err != nil {
		delete(a.logs, sid)
		return err
	}
	if _, still := a.logs[sid]; !still {
		// The UI closed the log panel while the stream was opening.
		stop()
		return nil
	}
	a.logs[sid] = stop
	return nil
}

func (a *App) StopLogs(sid string) {
	a.logsMu.Lock()
	stop := a.logs[sid]
	delete(a.logs, sid)
	a.logsMu.Unlock()
	if stop != nil {
		stop()
	}
}

func (a *App) SaveLogFile(name, content string) (string, error) {
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Save log", DefaultFilename: name,
		Filters: []runtime.FileFilter{{DisplayName: "Log (*.log)", Pattern: "*.log"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	if !strings.HasSuffix(strings.ToLower(path), ".log") && !strings.Contains(path[strings.LastIndexAny(path, `\/`)+1:], ".") {
		path += ".log"
	}
	return path, os.WriteFile(path, []byte(content), 0o644)
}
