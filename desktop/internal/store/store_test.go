package store

import (
	"strings"
	"testing"
)

func TestImportAndJumpHosts(t *testing.T) {
	s := &Store{dir: t.TempDir()}
	bastion, err := s.AddImported(Server{Name: "bastion", Host: "10.0.0.1", Username: "ops", AuthType: "agent", Password: "should-not-survive"})
	if err != nil {
		t.Fatal(err)
	}
	if bastion.Port != 22 || bastion.Password != "" {
		t.Errorf("defaults / secrets: %+v", bastion)
	}
	app, err := s.AddImported(Server{Name: "app", Host: "192.168.1.5", Port: 2200, Username: "deploy", AuthType: "password"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddImported(Server{Name: "dup", Host: "192.168.1.5", Port: 2200, Username: "deploy", AuthType: "agent"}); err == nil {
		t.Error("duplicate host/port/user accepted")
	}
	if _, err := s.AddImported(Server{Name: "weird", Host: "10.0.0.9", Username: "x", AuthType: "telnet"}); err != nil {
		t.Errorf("unknown auth type should fall back to agent: %v", err)
	}

	if err := s.SetJump(app.ID, bastion.ID); err != nil {
		t.Fatalf("set jump: %v", err)
	}
	if err := s.SetJump(bastion.ID, app.ID); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Errorf("loop not refused: %v", err)
	}
	if err := s.SetJump(app.ID, app.ID); err == nil {
		t.Error("server as its own jump host accepted")
	}
	if err := s.SetJump(app.ID, "missing"); err == nil {
		t.Error("missing jump host accepted")
	}
	if err := s.Remove(bastion.ID); err == nil || !strings.Contains(err.Error(), "jump host of app") {
		t.Errorf("removing a jump host in use: %v", err)
	}
	if err := s.Remove(app.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(bastion.ID); err != nil {
		t.Errorf("jump host no longer in use should be removable: %v", err)
	}
}

func TestSnippets(t *testing.T) {
	s := &Store{dir: t.TempDir()}
	if _, err := s.SaveSnippet(Snippet{Name: " ", Command: "ls"}); err == nil {
		t.Error("empty name accepted")
	}
	a, err := s.SaveSnippet(Snippet{Name: "Disk", Command: "df -h\n\n"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Command != "df -h" {
		t.Errorf("saved: %+v", a)
	}
	a.Command = "df -hT"
	if _, err := s.SaveSnippet(a); err != nil {
		t.Fatal(err)
	}
	if list := s.Snippets(); len(list) != 1 || list[0].Command != "df -hT" {
		t.Errorf("list after edit: %+v", list)
	}
	if err := s.DeleteSnippet(a.ID); err != nil || len(s.Snippets()) != 0 {
		t.Errorf("delete: %v %+v", err, s.Snippets())
	}
}
