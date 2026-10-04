package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.0", "1.1.9", true}, {"1.10.0", "1.9.0", true}, {"v2.0.0", "1.99.99", true},
		{"1.2.0", "1.2.0", false}, {"1.1.9", "1.2.0", false},
		{"1.2.0", "1.2.0-rc.1", true}, {"1.2.0-rc.1", "1.2.0", false}, {"1.2.0-rc.2", "1.2.0-rc.1", true},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
	if !IsDev("dev") || !IsDev("0.0.0-dev+abc") || IsDev("1.2.3") {
		t.Error("IsDev")
	}
}

// release serves a fake GitHub release: latest.json (+ signature) and the executable for this platform.
func release(t *testing.T, sk ed25519.PrivateKey, binary []byte, tamper func(m map[string]any, sig *[]byte, bin *[]byte)) *httptest.Server {
	sum := sha256.Sum256(binary)
	m := map[string]any{"version": "1.5.0", "notes": "new things", "assets": map[string]any{
		PlatformKey(): map[string]any{"name": "app-bin", "sha256": hex.EncodeToString(sum[:]), "size": len(binary)},
	}}
	bin := binary
	var sig []byte
	body, _ := json.Marshal(m)
	sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(sk, body)))
	if tamper != nil {
		tamper(m, &sig, &bin)
		if m["version"] != "1.5.0" { // manifest changed after signing
			body, _ = json.Marshal(m)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest/download/latest.json"):
			w.Write(body)
		case strings.HasSuffix(r.URL.Path, "/latest/download/latest.json.sig"):
			w.Write(sig)
		case strings.HasSuffix(r.URL.Path, "/download/v1.5.0/app-bin"):
			w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTestUpdater(t *testing.T, srv *httptest.Server, pk ed25519.PublicKey, current string) *Updater {
	dir := t.TempDir()
	exe := filepath.Join(dir, "app.exe")
	if err := os.WriteFile(exe, []byte("old version"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Updater{host: srv.URL + "/", current: current, exe: exe, client: srv.Client(), pubKey: pk}
}

func TestCheckDownloadApply(t *testing.T) {
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	binary := []byte("NEW VERSION " + strings.Repeat("x", 300000))
	srv := release(t, sk, binary, nil)
	defer srv.Close()
	u := newTestUpdater(t, srv, pk, "1.4.2")

	info, err := u.Check(context.Background())
	if err != nil || !info.Available || info.Latest != "1.5.0" || info.Notes != "new things" {
		t.Fatalf("check: %+v %v", info, err)
	}
	var last int64
	if err := u.Download(context.Background(), func(done, total int64) { last = done }); err != nil {
		t.Fatal(err)
	}
	if last != int64(len(binary)) || u.Staged() != "1.5.0" {
		t.Fatalf("progress %d, staged %q", last, u.Staged())
	}
	if err := u.Apply(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(u.exe); string(b) != string(binary) {
		t.Error("executable not replaced")
	}
	if b, _ := os.ReadFile(u.exe + ".old"); string(b) != "old version" {
		t.Error("previous executable not kept as .old")
	}
	u.CleanupOld()
	if _, err := os.Stat(u.exe + ".old"); !os.IsNotExist(err) {
		t.Error(".old not cleaned up")
	}
}

func TestRefusesUnsignedOrTampered(t *testing.T) {
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	_, otherSK, _ := ed25519.GenerateKey(rand.Reader)
	binary := []byte("NEW VERSION")

	// Signed with another key.
	srv := release(t, otherSK, binary, nil)
	u := newTestUpdater(t, srv, pk, "1.0.0")
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("foreign signature accepted: %v", err)
	}
	srv.Close()

	// Manifest edited after signing (e.g. pointing to an older or other build).
	srv = release(t, sk, binary, func(m map[string]any, _ *[]byte, _ *[]byte) { m["version"] = "9.9.9" })
	u = newTestUpdater(t, srv, pk, "1.0.0")
	if _, err := u.Check(context.Background()); err == nil {
		t.Error("tampered manifest accepted")
	}
	srv.Close()

	// Binary swapped on the server: signature fine, checksum not.
	srv = release(t, sk, binary, func(_ map[string]any, _ *[]byte, bin *[]byte) { *bin = []byte("EVIL VERSION") })
	u = newTestUpdater(t, srv, pk, "1.0.0")
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u.Download(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("swapped binary accepted: %v", err)
	}
	if u.Staged() != "" {
		t.Error("tampered download staged")
	}
	if b, _ := os.ReadFile(u.exe); string(b) != "old version" {
		t.Error("executable changed")
	}
	srv.Close()
}

func TestDevBuildsAreNotUpdated(t *testing.T) {
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	srv := release(t, sk, []byte("NEW"), nil)
	defer srv.Close()
	u := newTestUpdater(t, srv, pk, "dev")
	info, err := u.Check(context.Background())
	if err != nil || info.Available || !info.Dev {
		t.Fatalf("dev: %+v %v", info, err)
	}
	if err := u.Download(context.Background(), nil); err == nil {
		t.Error("dev build downloaded an update")
	}
}

func TestEmbeddedKeyIsValid(t *testing.T) {
	k, err := base64.StdEncoding.DecodeString(PublicKey)
	if err != nil || len(k) != ed25519.PublicKeySize {
		t.Fatalf("PublicKey is not a base64 Ed25519 key: %v", err)
	}
}
