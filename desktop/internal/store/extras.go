package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"serverdash/internal/secret"
)

// Snippet is a saved command, for every server (ServerID empty) or one server.
type Snippet struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Command  string `json:"command"`
	ServerID string `json:"serverId,omitempty"`
}

func (s *Store) snippetsFile() string { return filepath.Join(s.dir, "snippets.json") }

func (s *Store) Snippets() []Snippet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []Snippet
	_ = readJSON(s.snippetsFile(), &list)
	if list == nil {
		list = []Snippet{}
	}
	return list
}

func (s *Store) SaveSnippet(in Snippet) (Snippet, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Command = strings.TrimRight(in.Command, " \t\r\n")
	if in.Name == "" || strings.TrimSpace(in.Command) == "" {
		return in, errors.New("name and command are required")
	}
	if len(in.Command) > 20000 {
		return in, errors.New("the command is too long")
	}
	list := s.Snippets()
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.ID == "" {
		in.ID = newID()
		list = append(list, in)
	} else {
		i := slices.IndexFunc(list, func(x Snippet) bool { return x.ID == in.ID })
		if i < 0 {
			return in, errors.New("snippet not found")
		}
		list[i] = in
	}
	return in, writeJSON(s.snippetsFile(), list)
}

func (s *Store) DeleteSnippet(id string) error {
	list := slices.DeleteFunc(s.Snippets(), func(x Snippet) bool { return x.ID == id })
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.snippetsFile(), list)
}

// AddImported adds a server from an export file. Exports carry no secrets: a password or pasted-key server
// is saved without one and asks for it when edited; key files and the SSH agent work right away.
func (s *Store) AddImported(srv Server) (Server, error) {
	srv.Name = strings.TrimSpace(srv.Name)
	srv.Host = strings.TrimSpace(srv.Host)
	srv.Username = strings.TrimSpace(srv.Username)
	srv.Password, srv.PrivateKey, srv.Passphrase, srv.HostKey = "", "", "", ""
	if srv.Port == 0 {
		srv.Port = 22
	}
	if srv.Name == "" {
		srv.Name = srv.Host
	}
	if srv.Host == "" || srv.Username == "" || srv.Port < 1 || srv.Port > 65535 {
		return srv, errors.New("missing host, user or port")
	}
	if !slices.Contains([]string{"key", "keyPath", "agent", "password"}, srv.AuthType) {
		srv.AuthType = "agent"
	}
	s.mu.Lock()
	if slices.ContainsFunc(s.servers, func(x Server) bool {
		return strings.EqualFold(x.Host, srv.Host) && x.Port == srv.Port && x.Username == srv.Username
	}) {
		s.mu.Unlock()
		return srv, errors.New("already in the list")
	}
	srv.ID = newID()
	srv.JumpID = "" // linked after every server of the file is in (SetJump)
	s.servers = append(s.servers, srv)
	err := s.saveLocked()
	s.mu.Unlock()
	if err == nil {
		s.emit(srv.ID)
	}
	return srv, err
}

// SetJump changes the jump host of a server (used by the import).
func (s *Store) SetJump(id, jumpID string) error {
	s.mu.Lock()
	i := slices.IndexFunc(s.servers, func(x Server) bool { return x.ID == id })
	if i < 0 {
		s.mu.Unlock()
		return errors.New("server not found")
	}
	if err := s.checkJumpLocked(id, jumpID); err != nil {
		s.mu.Unlock()
		return err
	}
	s.servers[i].JumpID = jumpID
	err := s.saveLocked()
	s.mu.Unlock()
	if err == nil {
		s.emit(id)
	}
	return err
}

// CloneProfile is a saved list of changes for "Clone containers" (find and replace, values to set), applied
// again on the next clone. The changes often hold passwords, so the whole file is encrypted (see secret).
type CloneProfile struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Changes json.RawMessage `json:"changes"`
}

func (s *Store) cloneProfilesFile() string { return filepath.Join(s.dir, "clone-profiles.json") }

func (s *Store) CloneProfiles() ([]CloneProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cloneProfilesLocked()
}

func (s *Store) cloneProfilesLocked() ([]CloneProfile, error) {
	var file struct {
		Data string `json:"data"`
	}
	list := []CloneProfile{}
	if err := readJSON(s.cloneProfilesFile(), &file); err != nil || file.Data == "" {
		return list, err
	}
	plain, err := secret.Decrypt(file.Data)
	if err != nil {
		return list, fmt.Errorf("cannot decrypt the saved profiles: %w", err)
	}
	return list, json.Unmarshal([]byte(plain), &list)
}

func (s *Store) writeCloneProfilesLocked(list []CloneProfile) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	enc, err := secret.Encrypt(string(b))
	if err != nil {
		return err
	}
	return writeJSON(s.cloneProfilesFile(), map[string]string{"data": enc})
}

// SaveCloneProfile adds a profile, or replaces the one with the same name.
func (s *Store) SaveCloneProfile(in CloneProfile) (CloneProfile, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return in, errors.New("enter a name")
	}
	if len(in.Changes) == 0 || !json.Valid(in.Changes) {
		return in, errors.New("no changes to save")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.cloneProfilesLocked()
	if err != nil {
		return in, err
	}
	if i := slices.IndexFunc(list, func(x CloneProfile) bool { return strings.EqualFold(x.Name, in.Name) }); i >= 0 {
		in.ID = list[i].ID
		list[i] = in
	} else {
		in.ID = newID()
		list = append(list, in)
	}
	return in, s.writeCloneProfilesLocked(list)
}

func (s *Store) DeleteCloneProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.cloneProfilesLocked()
	if err != nil {
		return err
	}
	return s.writeCloneProfilesLocked(slices.DeleteFunc(list, func(x CloneProfile) bool { return x.ID == id }))
}
