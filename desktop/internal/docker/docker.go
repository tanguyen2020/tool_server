// Package docker controls containers and streams their logs over SSH.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

var (
	// Container names/IDs only allow safe characters -> no shell injection possible.
	containerRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	tsRe        = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{1,9})?Z$`)
	actions     = map[string]bool{"start": true, "stop": true, "restart": true}
)

// ValidateName checks a container name/ID before it is used in a shell command.
func ValidateName(container string) error { return validate(container) }

func validate(container string) error {
	if !containerRe.MatchString(container) {
		return errors.New("invalid container name")
	}
	return nil
}

func Action(ctx context.Context, conn *sshpool.Conn, srv store.Server, container, action string) error {
	if !actions[action] {
		return errors.New("invalid action")
	}
	if err := validate(container); err != nil {
		return err
	}
	_, err := run(ctx, conn, fmt.Sprintf("%s %s %s", sshpool.DockerBin(srv), action, container), action, 90*time.Second)
	return err
}

// Inspect returns the `docker inspect` object of one container.
func Inspect(ctx context.Context, conn *sshpool.Conn, srv store.Server, container string) (json.RawMessage, error) {
	if err := validate(container); err != nil {
		return nil, err
	}
	out, err := run(ctx, conn, fmt.Sprintf("%s inspect --type container %s", sshpool.DockerBin(srv), container), "inspect", 30*time.Second)
	if err != nil {
		return nil, err
	}
	var list []json.RawMessage
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) == 0 {
		return nil, errors.New("unexpected docker inspect output")
	}
	return list[0], nil
}

// run executes a docker command and turns a non-zero exit into an error carrying docker's message.
func run(ctx context.Context, conn *sshpool.Conn, cmd, name string, timeout time.Duration) (string, error) {
	res, err := conn.Exec(ctx, cmd, timeout)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr + res.Stdout)
		if msg == "" {
			msg = fmt.Sprintf("docker %s failed (exit %d)", name, res.Code)
		}
		return "", errors.New(msg)
	}
	return res.Stdout, nil
}

const maxBuffer = 512 * 1024

// Logs streams `docker logs -f`, batching output every 100ms so the UI is not flooded by bursts.
// onData/onEnd are called from separate goroutines. Returns a function that stops the stream.
func Logs(conn *sshpool.Conn, srv store.Server, container string, tail int, since string, onData func(string), onEnd func(string)) (func(), error) {
	if err := validate(container); err != nil {
		return nil, err
	}
	from := fmt.Sprintf("--tail %d", max(0, min(tail, 5000)))
	// Resuming after a container restart: continue from the last timestamp received.
	if tsRe.MatchString(since) {
		from = "--since " + since
	}
	// `exec` replaces the shell with docker -> the stop signal reaches docker logs directly.
	stream, err := conn.Stream(fmt.Sprintf("exec %s logs -f --timestamps %s %s 2>&1", sshpool.DockerBin(srv), from, container))
	if err != nil {
		return nil, err
	}

	var (
		mu      sync.Mutex
		buf     strings.Builder
		dropped int
		stopped bool
	)
	done := make(chan struct{})
	flush := func() {
		mu.Lock()
		data := buf.String()
		buf.Reset()
		d := dropped
		dropped = 0
		mu.Unlock()
		if d > 0 {
			onData(fmt.Sprintf("\n[dashboard] Skipped %d bytes of log output (too much data)\n", d))
		}
		if data != "" {
			onData(data)
		}
	}

	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				flush()
			case <-done:
				return
			}
		}
	}()

	go func() {
		chunk := make([]byte, 32*1024)
		for {
			n, err := stream.Read(chunk)
			if n > 0 {
				mu.Lock()
				if buf.Len() > maxBuffer {
					dropped += n
				} else {
					buf.Write(chunk[:n])
				}
				mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		werr := stream.Wait()
		close(done)
		mu.Lock()
		wasStopped := stopped
		mu.Unlock()
		if wasStopped {
			return
		}
		flush()
		reason := "Container stopped or the stream ended"
		if werr != nil && !strings.Contains(werr.Error(), "exited without exit status") {
			reason = "docker logs ended: " + werr.Error()
		}
		onEnd(reason)
	}()

	return func() {
		mu.Lock()
		stopped = true
		mu.Unlock()
		stream.Stop()
	}, nil
}

// DiskUsage is one row of `docker system df`.
type DiskUsage struct {
	Type        string `json:"type"`
	Total       string `json:"total"`
	Active      string `json:"active"`
	Size        string `json:"size"`
	Reclaimable string `json:"reclaimable"`
}

// SystemDF reports the space used by images, containers, volumes and the build cache.
func SystemDF(ctx context.Context, conn *sshpool.Conn, srv store.Server) ([]DiskUsage, error) {
	out, err := run(ctx, conn, sshpool.DockerBin(srv)+" system df --format '{{json .}}'", "system df", 120*time.Second)
	if err != nil {
		return nil, err
	}
	var rows []DiskUsage
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var r struct{ Type, TotalCount, Active, Size, Reclaimable string }
		if json.Unmarshal([]byte(l), &r) == nil && r.Type != "" {
			rows = append(rows, DiskUsage{r.Type, r.TotalCount, r.Active, r.Size, r.Reclaimable})
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("unexpected docker system df output")
	}
	return rows, nil
}

// Remove deletes a container (docker rm). force also stops it first when it is running.
// Volumes and the image are kept.
func Remove(ctx context.Context, conn *sshpool.Conn, srv store.Server, container string, force bool) error {
	if err := validate(container); err != nil {
		return err
	}
	flag := ""
	if force {
		flag = " -f"
	}
	_, err := run(ctx, conn, fmt.Sprintf("%s rm%s %s", sshpool.DockerBin(srv), flag, container), "rm", 90*time.Second)
	return err
}
