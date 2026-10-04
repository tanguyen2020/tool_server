// Package store persists the server list and settings in the user's config directory
// (Windows: %AppData%\ServerDashboard). Secrets are always encrypted before being written.
package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"serverdash/internal/secret"
	"serverdash/internal/theme"
)

type Server struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Group          string `json:"group,omitempty"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	AuthType       string `json:"authType"` // key | keyPath | agent | password
	PrivateKeyPath string `json:"privateKeyPath,omitempty"`
	UseSudo        bool   `json:"useSudo"`
	HostKey        string `json:"hostKey,omitempty"`
	JumpID         string `json:"jumpId,omitempty"` // reached through this saved server (bastion), like ssh -J
	// The fields below are always stored encrypted.
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// ServerInfo is what gets sent to the UI (no secrets).
type ServerInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Group          string `json:"group,omitempty"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	AuthType       string `json:"authType"`
	PrivateKeyPath string `json:"privateKeyPath,omitempty"`
	UseSudo        bool   `json:"useSudo"`
	HostKey        string `json:"hostKey,omitempty"`
	JumpID         string `json:"jumpId,omitempty"`
	HasPassword    bool   `json:"hasPassword"`
	HasPrivateKey  bool   `json:"hasPrivateKey"`
	HasPassphrase  bool   `json:"hasPassphrase"`
}

// Input is the add/edit server form. An empty secret field keeps the stored value.
type Input struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Group          string `json:"group"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	AuthType       string `json:"authType"`
	PrivateKeyPath string `json:"privateKeyPath"`
	UseSudo        bool   `json:"useSudo"`
	JumpID         string `json:"jumpId"`
	Password       string `json:"password"`
	PrivateKey     string `json:"privateKey"`
	Passphrase     string `json:"passphrase"`
}

// Credentials is a decrypted config, used internally only to open SSH connections.
type Credentials struct {
	Server
	PlainPassword   string
	PlainPrivateKey []byte
	PlainPassphrase string
}

// Thresholds drive the threshold alerts. Percentages are 1–100.
type Thresholds struct {
	CPU            float64 `json:"cpu"`
	Memory         float64 `json:"memory"`
	Disk           float64 `json:"disk"` // also used for inodes
	IOWait         float64 `json:"iowait"`
	SustainMinutes float64 `json:"sustainMinutes"` // CPU, memory and I/O wait must stay high this long
}

var DefaultThresholds = Thresholds{CPU: 90, Memory: 90, Disk: 90, IOWait: 30, SustainMinutes: 5}

type Settings struct {
	Notifications bool       `json:"notifications"`
	Alerts        Thresholds `json:"alerts"`
	Theme         string     `json:"theme"` // system, light or dark
	// UI holds small view preferences of the page (chosen view, sort, last tab…), kept here rather than only
	// in the WebView storage, which loses recent writes when the app is killed.
	UI map[string]string `json:"ui,omitempty"`
}

type Store struct {
	mu        sync.RWMutex
	dir       string
	servers   []Server
	settings  Settings
	listeners []func(id string)
}

func Open() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "ServerDashboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	secret.KeyDir = dir
	// Defaults stay in place for keys missing from an older settings.json.
	s := &Store{dir: dir, settings: Settings{Notifications: true, Alerts: DefaultThresholds}}
	if err := readJSON(filepath.Join(dir, "servers.json"), &s.servers); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "settings.json"), &s.settings); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

func readJSON(file string, v any) error {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(file string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// OnChange registers a callback for when a server is added, edited or removed.
func (s *Store) OnChange(fn func(id string)) {
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

func (s *Store) emit(id string) {
	s.mu.RLock()
	ls := slices.Clone(s.listeners)
	s.mu.RUnlock()
	for _, fn := range ls {
		fn(id)
	}
}

func (s *Store) saveLocked() error {
	return writeJSON(filepath.Join(s.dir, "servers.json"), s.servers)
}

func (s *Store) List() []Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.servers)
}

func (s *Store) Get(id string) (Server, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, srv := range s.servers {
		if srv.ID == id {
			return srv, true
		}
	}
	return Server{}, false
}

func ToPublic(s Server) ServerInfo {
	return ServerInfo{
		ID: s.ID, Name: s.Name, Group: s.Group, Host: s.Host, Port: s.Port, Username: s.Username,
		AuthType: s.AuthType, PrivateKeyPath: s.PrivateKeyPath, UseSudo: s.UseSudo, HostKey: s.HostKey, JumpID: s.JumpID,
		HasPassword: s.Password != "", HasPrivateKey: s.PrivateKey != "", HasPassphrase: s.Passphrase != "",
	}
}

func (s *Store) ListPublic() []ServerInfo {
	list := s.List()
	out := make([]ServerInfo, len(list))
	for i, srv := range list {
		out[i] = ToPublic(srv)
	}
	return out
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// Decrypt decrypts the secrets and reads the key file when needed.
func Decrypt(srv Server) (Credentials, error) {
	c := Credentials{Server: srv}
	var err error
	if c.PlainPassword, err = secret.Decrypt(srv.Password); err != nil {
		return c, fmt.Errorf("cannot decrypt password: %w", err)
	}
	if c.PlainPassphrase, err = secret.Decrypt(srv.Passphrase); err != nil {
		return c, fmt.Errorf("cannot decrypt passphrase: %w", err)
	}
	switch srv.AuthType {
	case "key":
		key, err := secret.Decrypt(srv.PrivateKey)
		if err != nil {
			return c, fmt.Errorf("cannot decrypt private key: %w", err)
		}
		c.PlainPrivateKey = []byte(key)
	case "keyPath":
		key, err := os.ReadFile(expandHome(srv.PrivateKeyPath))
		if err != nil {
			return c, fmt.Errorf("cannot read key file: %w", err)
		}
		c.PlainPrivateKey = key
	}
	return c, nil
}

func normalize(in Input, existing Server) (Server, error) {
	s := existing
	s.Name = strings.TrimSpace(in.Name)
	s.Group = strings.TrimSpace(in.Group)
	s.Host = strings.TrimSpace(in.Host)
	s.Port = in.Port
	s.Username = strings.TrimSpace(in.Username)
	s.UseSudo = in.UseSudo
	s.AuthType = in.AuthType
	s.PrivateKeyPath = strings.TrimSpace(in.PrivateKeyPath)
	s.JumpID = in.JumpID
	if s.Port == 0 {
		s.Port = 22
	}
	if s.Name == "" {
		s.Name = s.Host
	}
	if s.Host == "" || s.Username == "" {
		return s, errors.New("host and user are required")
	}
	if s.Port < 1 || s.Port > 65535 {
		return s, errors.New("invalid port")
	}
	if !slices.Contains([]string{"key", "keyPath", "agent", "password"}, s.AuthType) {
		return s, errors.New("invalid authentication type")
	}
	for _, f := range []struct {
		plain string
		dst   *string
	}{{in.Password, &s.Password}, {in.PrivateKey, &s.PrivateKey}, {in.Passphrase, &s.Passphrase}} {
		if f.plain == "" {
			continue
		}
		enc, err := secret.Encrypt(f.plain)
		if err != nil {
			return s, fmt.Errorf("cannot encrypt secret: %w", err)
		}
		*f.dst = enc
	}
	// Drop secrets that no longer apply after changing the auth type.
	if s.AuthType != "password" {
		s.Password = ""
	}
	if s.AuthType != "key" {
		s.PrivateKey = ""
	}
	if s.AuthType != "key" && s.AuthType != "keyPath" {
		s.Passphrase = ""
	}
	if s.AuthType != "keyPath" {
		s.PrivateKeyPath = ""
	}
	switch {
	case s.AuthType == "password" && s.Password == "":
		return s, errors.New("SSH password is required")
	case s.AuthType == "key" && s.PrivateKey == "":
		return s, errors.New("paste the private key content")
	case s.AuthType == "keyPath" && s.PrivateKeyPath == "":
		return s, errors.New("select a private key file")
	}
	// A new host/port or route means the host key has to be trusted again.
	if existing.Host != s.Host || existing.Port != s.Port || existing.JumpID != s.JumpID {
		s.HostKey = ""
	}
	return s, nil
}

// Draft builds a temporary (unsaved) config for testing a connection.
func (s *Store) Draft(in Input) (Server, error) {
	existing, _ := s.Get(in.ID)
	srv, err := normalize(in, existing)
	if err == nil {
		s.mu.RLock()
		err = s.checkJumpLocked(in.ID, srv.JumpID)
		s.mu.RUnlock()
	}
	return srv, err
}

// checkJumpLocked rejects a jump host that does not exist or that would loop back to the server itself.
func (s *Store) checkJumpLocked(id, jumpID string) error {
	for depth := 0; jumpID != ""; depth++ {
		if jumpID == id && id != "" {
			return errors.New("a server cannot be its own jump host (loop)")
		}
		if depth > 4 {
			return errors.New("too many jump hosts in a row (at most 4)")
		}
		idx := slices.IndexFunc(s.servers, func(x Server) bool { return x.ID == jumpID })
		if idx < 0 {
			return errors.New("the jump host no longer exists")
		}
		jumpID = s.servers[idx].JumpID
	}
	return nil
}

// Save adds a new server (empty ID) or updates an existing one.
func (s *Store) Save(in Input) (Server, error) {
	s.mu.Lock()
	idx := slices.IndexFunc(s.servers, func(x Server) bool { return x.ID == in.ID && in.ID != "" })
	var existing Server
	if idx >= 0 {
		existing = s.servers[idx]
	} else if in.ID != "" {
		s.mu.Unlock()
		return Server{}, errors.New("server not found")
	}
	srv, err := normalize(in, existing)
	if err == nil {
		err = s.checkJumpLocked(in.ID, srv.JumpID)
	}
	if err != nil {
		s.mu.Unlock()
		return srv, err
	}
	if idx >= 0 {
		s.servers[idx] = srv
	} else {
		srv.ID = newID()
		s.servers = append(s.servers, srv)
	}
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return srv, err
	}
	s.emit(srv.ID)
	return srv, nil
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	for i := range s.servers {
		if s.servers[i].JumpID == id {
			s.mu.Unlock()
			return fmt.Errorf("%s is the jump host of %s: change that server first", s.nameLocked(id), s.servers[i].Name)
		}
	}
	s.servers = slices.DeleteFunc(s.servers, func(x Server) bool { return x.ID == id })
	err := s.saveLocked()
	s.mu.Unlock()
	s.emit(id)
	return err
}

func (s *Store) nameLocked(id string) string {
	for _, x := range s.servers {
		if x.ID == id {
			return x.Name
		}
	}
	return id
}

func (s *Store) SetHostKey(id, hostKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.servers {
		if s.servers[i].ID == id {
			s.servers[i].HostKey = hostKey
			_ = s.saveLocked()
		}
	}
}

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *Store) SetSettings(v Settings) error {
	a := v.Alerts
	for _, p := range []float64{a.CPU, a.Memory, a.Disk, a.IOWait} {
		if p < 1 || p > 100 {
			return errors.New("alert thresholds must be between 1 and 100%")
		}
	}
	if a.SustainMinutes < 0 || a.SustainMinutes > 120 {
		return errors.New("sustain time must be between 0 and 120 minutes")
	}
	if !theme.Valid(v.Theme) {
		return errors.New("invalid theme")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v.UI = s.settings.UI // owned by SetUIPref: a stale copy from the page must not overwrite it
	s.settings = v
	return writeJSON(filepath.Join(s.dir, "settings.json"), v)
}

// SetUIPref stores one view preference (empty value removes it).
func (s *Store) SetUIPref(key, value string) error {
	if key == "" || len(key) > 120 || len(value) > 8192 {
		return errors.New("invalid preference")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings.UI == nil {
		s.settings.UI = map[string]string{}
	}
	if value == "" {
		delete(s.settings.UI, key)
	} else {
		if _, ok := s.settings.UI[key]; !ok && len(s.settings.UI) >= 500 {
			return errors.New("too many preferences")
		}
		s.settings.UI[key] = value
	}
	return writeJSON(filepath.Join(s.dir, "settings.json"), s.settings)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
