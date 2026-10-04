// Package activity keeps a local record of the actions done from the app (who did what, when, result),
// as JSON lines in activity.log next to the settings. Old entries are dropped past maxEntries.
package activity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxEntries = 5000

type Entry struct {
	Time     int64  `json:"time"` // unix ms
	User     string `json:"user"` // Windows user running the app
	ServerID string `json:"serverId,omitempty"`
	Server   string `json:"server,omitempty"`
	Action   string `json:"action"`
	Target   string `json:"target,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type Log struct {
	mu    sync.Mutex
	file  string
	user  string
	count int // lines in the file (approximate until the first read)
}

func Open(dir string) *Log {
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	l := &Log{file: filepath.Join(dir, "activity.log"), user: user}
	l.count = len(l.read())
	return l
}

// Add records one action; err nil means it succeeded.
func (l *Log) Add(serverID, server, action, target string, err error) {
	e := Entry{Time: time.Now().UnixMilli(), User: l.user, ServerID: serverID, Server: server, Action: action, Target: target, OK: err == nil}
	if err != nil {
		e.Error = err.Error()
	}
	b, _ := json.Marshal(e)
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ferr := os.OpenFile(l.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	l.count++
	if l.count > maxEntries+500 {
		l.trimLocked()
	}
}

func (l *Log) read() []Entry {
	b, err := os.ReadFile(l.file)
	if err != nil {
		return nil
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (l *Log) trimLocked() {
	all := l.read()
	if len(all) > maxEntries {
		all = all[len(all)-maxEntries:]
	}
	var buf bytes.Buffer
	for _, e := range all {
		b, _ := json.Marshal(e)
		buf.Write(append(b, '\n'))
	}
	tmp := l.file + ".tmp"
	if os.WriteFile(tmp, buf.Bytes(), 0o600) == nil {
		_ = os.Rename(tmp, l.file)
	}
	l.count = len(all)
}

// List returns the newest entries first, for one server (serverID) or all of them (empty).
func (l *Log) List(serverID string, limit int) []Entry {
	l.mu.Lock()
	all := l.read()
	l.mu.Unlock()
	out := []Entry{}
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		if serverID == "" || all[i].ServerID == serverID {
			out = append(out, all[i])
		}
	}
	return out
}
