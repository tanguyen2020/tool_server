package main

import (
	"errors"
	"fmt"
	"path"

	"serverdash/internal/ops"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

// audit records an action in the activity log.
func (a *App) audit(serverID, action, target string, err error) {
	if a.act == nil {
		return
	}
	name := ""
	if srv, ok := a.st.Get(serverID); ok {
		name = srv.Name
	}
	a.act.Add(serverID, name, action, target, err)
}

// server returns a saved server and its connection.
func (a *App) server(serverID string) (store.Server, *sshpool.Conn, error) {
	srv, ok := a.st.Get(serverID)
	if !ok {
		return srv, nil, errors.New("server not found")
	}
	conn, err := a.pool.Get(serverID)
	return srv, conn, err
}

// ------------------------------------------------------------------ compose

func (a *App) ComposeInfo(serverID, project string) (ops.ComposeProject, error) {
	srv, conn, err := a.server(serverID)
	if err != nil {
		return ops.ComposeProject{}, err
	}
	return ops.ComposeInfo(a.ctx, conn, srv, project)
}

// ------------------------------------------------------------------ Docker cleanup

func (a *App) DockerPrune(serverID, kind string) (string, error) {
	srv, conn, err := a.server(serverID)
	if err != nil {
		return "", err
	}
	out, err := ops.Prune(a.ctx, conn, srv, kind)
	a.audit(serverID, "docker cleanup", kind, err)
	go a.mon.Collect(serverID)
	return out, err
}

// ------------------------------------------------------------------ systemd

func (a *App) Services(serverID string) ([]ops.Service, error) {
	_, conn, err := a.server(serverID)
	if err != nil {
		return nil, err
	}
	return ops.Services(a.ctx, conn)
}

func (a *App) ServiceAction(serverID, name, action string) error {
	_, conn, err := a.server(serverID)
	if err != nil {
		return err
	}
	err = ops.ServiceAction(a.ctx, conn, name, action)
	a.audit(serverID, "service "+action, name, err)
	return err
}

func (a *App) ServiceStatus(serverID, name string) (string, error) {
	_, conn, err := a.server(serverID)
	if err != nil {
		return "", err
	}
	return ops.ServiceStatus(a.ctx, conn, name)
}

// ------------------------------------------------------------------ ports & disk usage

type PortsResult struct {
	Ports []ops.Port `json:"ports"`
	// WithProcesses is false when the user cannot read other users' process names (no root / sudo).
	WithProcesses bool `json:"withProcesses"`
}

func (a *App) ListeningPorts(serverID string) (PortsResult, error) {
	_, conn, err := a.server(serverID)
	if err != nil {
		return PortsResult{}, err
	}
	ports, procs, err := ops.ListeningPorts(a.ctx, conn)
	return PortsResult{Ports: ports, WithProcesses: procs}, err
}

type DuResult struct {
	Path  string        `json:"path"`
	Total int64         `json:"total"`
	Dirs  []ops.DirSize `json:"dirs"`
}

func (a *App) DiskUsageAt(serverID, dir string) (DuResult, error) {
	_, conn, err := a.server(serverID)
	if err != nil {
		return DuResult{}, err
	}
	dir = path.Clean("/" + dir)
	total, dirs, err := ops.DiskUsageAt(a.ctx, conn, dir)
	return DuResult{Path: dir, Total: total, Dirs: dirs}, err
}

// ------------------------------------------------------------------ terminal tasks

type TaskInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// OpenTask runs an operation in a terminal so its output and prompts are visible:
//   - compose:<update|up|restart|stop|start|down> with the project name
//   - upgrade (apt-get update && upgrade, answers typed by the user)
//   - journal with a service name (follows its journal)
//   - shell with a folder (a shell opened in that folder)
func (a *App) OpenTask(serverID, kind, arg string, cols, rows int) (TaskInfo, error) {
	srv, conn, err := a.server(serverID)
	if err != nil {
		return TaskInfo{}, err
	}
	var script, title string
	audit := ""
	switch {
	case len(kind) > 8 && kind[:8] == "compose:":
		p, err := ops.ComposeInfo(a.ctx, conn, srv, arg)
		if err != nil {
			return TaskInfo{}, err
		}
		if script, title, err = ops.ComposeScript(srv, p, kind[8:]); err != nil {
			return TaskInfo{}, err
		}
		audit = "compose " + kind[8:]
	case kind == "upgrade":
		script, title, audit = ops.UpgradeScript(), "apt upgrade", "package upgrade"
	case kind == "journal":
		if script, err = ops.JournalScript(arg); err != nil {
			return TaskInfo{}, err
		}
		title = "journal " + arg
	case kind == "shell":
		if script, err = ops.ShellAt(arg); err != nil {
			return TaskInfo{}, err
		}
		title = fmt.Sprintf("%s:%s", srv.Name, arg)
	default:
		return TaskInfo{}, errors.New("unknown task")
	}
	id, err := a.openPTY(serverID, script, cols, rows)
	if audit != "" {
		a.audit(serverID, audit, arg, err)
	}
	return TaskInfo{ID: id, Title: title}, err
}
