// Package updater keeps the app up to date from the GitHub releases of the project.
//
// Every release carries latest.json (version, notes, and the name, size and SHA-256 of the executable for
// each platform) and latest.json.sig, an Ed25519 signature of that file made in CI with the release
// signing key. The app embeds only the public key: it installs nothing whose manifest is not signed by
// that key, and nothing whose bytes do not match the signed SHA-256.
//
// Installing swaps the executable in place: the running file is renamed to <exe>.old (allowed on Windows,
// macOS and Linux), the verified download takes its name, and the app restarts. The .old file is
// removed at the next start.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo publishes the releases (it must be public for the app to download them).
const Repo = "tanguyen2020/tool_server"

// PublicKey verifies latest.json.sig (Ed25519, base64). The private half is the UPDATE_SIGNING_KEY
// secret of the repository and never leaves CI.
const PublicKey = "EUaYdVQ6scGejki/BJqKSZcAjo+9WtOz6iMi4oY9J5g="

type Asset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Version   string           `json:"version"`
	Notes     string           `json:"notes"`
	Published string           `json:"published"`
	Assets    map[string]Asset `json:"assets"`
}

// Info is what the UI shows.
type Info struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	Notes     string `json:"notes"`
	Page      string `json:"page"`   // release page
	Staged    string `json:"staged"` // version downloaded and ready to install
	Dev       bool   `json:"dev"`    // development build: never replaced automatically
}

// PlatformKey names this build in the manifest: windows-amd64, linux-amd64, darwin-universal.
func PlatformKey() string {
	if runtime.GOOS == "darwin" {
		return "darwin-universal"
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

// Newer reports whether version a is higher than b (x.y.z; a pre-release suffix sorts lower).
func Newer(a, b string) bool {
	pa, pb := parse(a), parse(b)
	for i := 0; i < 3; i++ {
		if pa.n[i] != pb.n[i] {
			return pa.n[i] > pb.n[i]
		}
	}
	return pa.pre == "" && pb.pre != "" || (pa.pre != "" && pb.pre != "" && pa.pre > pb.pre)
}

type semver struct {
	n   [3]int
	pre string
}

func parse(v string) semver {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var s semver
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		s.pre, v = v[i+1:], v[:i]
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		s.n[i], _ = strconv.Atoi(part)
	}
	return s
}

// IsDev is true for local builds (no version stamped in by the release build).
func IsDev(version string) bool {
	return version == "" || version == "dev" || strings.Contains(version, "dev")
}

type Updater struct {
	host    string // https://github.com/ (a test server in tests)
	current string
	exe     string
	client  *http.Client
	pubKey  ed25519.PublicKey

	mu       sync.Mutex
	manifest *Manifest
	staged   string // version waiting in <exe>.update
}

func New(current string) *Updater {
	exe, err := os.Executable()
	if err == nil {
		if real, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = real
		}
	}
	key, _ := base64.StdEncoding.DecodeString(PublicKey)
	host := "https://github.com/"
	// A mirror or a test server can stand in for GitHub; the signature check still applies.
	if h := os.Getenv("SERVERDASH_UPDATE_HOST"); h != "" {
		host = strings.TrimSuffix(h, "/") + "/"
	}
	return &Updater{host: host, current: current, exe: exe, client: &http.Client{Timeout: 10 * time.Minute}, pubKey: key}
}

func (u *Updater) Version() string { return u.current }

func (u *Updater) stagedPath() string { return u.exe + ".update" }

// CleanupOld removes the executable replaced by the last update.
func (u *Updater) CleanupOld() {
	if u.exe != "" {
		_ = os.Remove(u.exe + ".old")
		// A download interrupted by closing the app is not trusted later.
		_ = os.Remove(u.stagedPath())
	}
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ServerDashboard/"+u.current)
	res, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, errors.New("no release published yet")
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s", res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}

// verify checks the manifest signature with the embedded public key.
func (u *Updater) verify(manifest, sig []byte) error {
	if len(u.pubKey) != ed25519.PublicKeySize {
		return errors.New("this build has no update key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(u.pubKey, manifest, raw) {
		return errors.New("the update is not signed with the project's key: refused")
	}
	return nil
}

// Check downloads and verifies the latest release manifest.
func (u *Updater) Check(ctx context.Context) (Info, error) {
	info := Info{Current: u.current, Dev: IsDev(u.current), Page: "https://github.com/" + Repo + "/releases/latest"}
	base := u.host + Repo + "/releases/latest/download/"
	body, err := u.get(ctx, base+"latest.json", 1<<20)
	if err != nil {
		return info, err
	}
	sig, err := u.get(ctx, base+"latest.json.sig", 4096)
	if err != nil {
		return info, err
	}
	if err := u.verify(body, sig); err != nil {
		return info, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return info, errors.New("unreadable release manifest")
	}
	u.mu.Lock()
	u.manifest = &m
	info.Staged = u.staged
	u.mu.Unlock()
	info.Latest, info.Notes = m.Version, m.Notes
	info.Page = "https://github.com/" + Repo + "/releases/tag/v" + m.Version
	_, hasAsset := m.Assets[PlatformKey()]
	info.Available = hasAsset && !info.Dev && Newer(m.Version, u.current)
	return info, nil
}

// Download fetches the executable of the checked release for this platform and verifies it.
// progress is called with the bytes received so far and the total.
func (u *Updater) Download(ctx context.Context, progress func(done, total int64)) error {
	u.mu.Lock()
	m := u.manifest
	u.mu.Unlock()
	if m == nil {
		return errors.New("check for updates first")
	}
	if IsDev(u.current) {
		return errors.New("development builds are not updated")
	}
	asset, ok := m.Assets[PlatformKey()]
	if !ok {
		return fmt.Errorf("version %s has no build for %s", m.Version, PlatformKey())
	}
	url := fmt.Sprintf("%s%s/releases/download/v%s/%s", u.host, Repo, m.Version, asset.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ServerDashboard/"+u.current)
	res, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", res.Status)
	}
	tmp := u.stagedPath() + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("cannot write next to the app (%s): %w", filepath.Dir(u.exe), err)
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 256*1024)
	last := time.Time{}
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(tmp)
				return err
			}
			h.Write(buf[:n])
			done += int64(n)
			if done > asset.Size+1 {
				f.Close()
				os.Remove(tmp)
				return errors.New("download larger than announced: refused")
			}
			if progress != nil && time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				progress(done, asset.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("download interrupted: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if done != asset.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), asset.SHA256) {
		os.Remove(tmp)
		return errors.New("the download does not match the signed checksum: refused")
	}
	if progress != nil {
		progress(done, asset.Size)
	}
	if err := os.Rename(tmp, u.stagedPath()); err != nil {
		os.Remove(tmp)
		return err
	}
	u.mu.Lock()
	u.staged = m.Version
	u.mu.Unlock()
	return nil
}

// Staged is the version downloaded and waiting to be installed ("" when none).
func (u *Updater) Staged() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.staged
}

// Apply puts the downloaded version in place of the running executable.
func (u *Updater) Apply() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.staged == "" {
		return errors.New("no update downloaded")
	}
	old := u.exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(u.exe, old); err != nil {
		return fmt.Errorf("cannot replace the app (%s): %w", u.exe, err)
	}
	if err := os.Rename(u.stagedPath(), u.exe); err != nil {
		_ = os.Rename(old, u.exe) // put the running version back
		return fmt.Errorf("cannot install the update: %w", err)
	}
	u.staged = ""
	return nil
}

// Relaunch starts the (new) executable; it waits for this process to exit before opening its window.
func (u *Updater) Relaunch() error {
	wait := "--wait-pid=" + strconv.Itoa(os.Getpid())
	cmd := exec.Command(u.exe, wait)
	// On macOS start the .app bundle (Dock icon, menu) rather than the bare executable inside it.
	if i := strings.Index(u.exe, ".app/Contents/MacOS/"); runtime.GOOS == "darwin" && i > 0 {
		cmd = exec.Command("open", "-n", u.exe[:i+4], "--args", wait)
	}
	cmd.Dir = filepath.Dir(u.exe)
	detach(cmd)
	return cmd.Start()
}

// WaitForParent handles --wait-pid=N: block (up to 20 s) until the previous instance has exited, so the
// single-instance lock does not hand the start over to the closing window.
func WaitForParent(args []string) {
	for _, a := range args {
		if pid, ok := strings.CutPrefix(a, "--wait-pid="); ok {
			if n, err := strconv.Atoi(pid); err == nil {
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) && processAlive(n) {
					time.Sleep(150 * time.Millisecond)
				}
			}
		}
	}
}
