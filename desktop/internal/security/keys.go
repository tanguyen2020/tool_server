package security

import (
	"bufio"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// KeyUser is an account of the server.
type KeyUser struct {
	Name     string `json:"name"`
	UID      int    `json:"uid"`
	Home     string `json:"home"`
	Shell    string `json:"shell"`
	CanLogin bool   `json:"canLogin"` // has a login shell (keys of other accounts still allow tunnels)
}

// KeyFile is an authorized_keys file of one user.
type KeyFile struct {
	User     string    `json:"user"`
	Path     string    `json:"path"`
	ModTime  int64     `json:"modTime"` // unix ms
	Readable bool      `json:"readable"`
	TooBig   bool      `json:"tooBig"`
	Keys     []AuthKey `json:"keys"`
	content  []byte
}

// AuthKey is one line of an authorized_keys file.
type AuthKey struct {
	Line        int      `json:"line"`
	Type        string   `json:"type"` // ssh-ed25519, ssh-rsa…
	Bits        int      `json:"bits,omitempty"`
	Fingerprint string   `json:"fingerprint"`
	Comment     string   `json:"comment"`
	Options     []string `json:"options"` // from="…", command="…", no-pty…
	Weak        string   `json:"weak,omitempty"`
	Invalid     string   `json:"invalid,omitempty"` // a line sshd cannot use (shown cut)
}

type Keys struct {
	Root     bool      `json:"root"` // read as root: every user's files are listed
	User     string    `json:"user"` // the SSH user of the app
	Patterns []string  `json:"patterns"`
	Users    []KeyUser `json:"users"`
	Files    []KeyFile `json:"files"`
	AppKey   string    `json:"appKey"` // fingerprint of the key the app signs in with, when known
	Docker   bool      `json:"docker"` // the docker group exists (a new user can be put in it)
}

// NewUser is a user created with its first key.
type NewUser struct {
	Create bool `json:"create"`
	Docker bool `json:"docker"` // add to the docker group (as powerful as root)
	Sudo   bool `json:"sudo"`   // may run every command as root with sudo, without a password
}

const maxKeysFile = 256 * 1024

const keysBody = `export LC_ALL=C PATH="$PATH:/usr/sbin:/sbin"
set -f
AKF=$(sshd -T 2>/dev/null | awk '$1=="authorizedkeysfile"{$1=""; sub(/^ +/, ""); print; exit}')
[ -n "$AKF" ] || AKF=".ssh/authorized_keys .ssh/authorized_keys2"
printf '@@AKF\t%s\n' "$AKF"
getent group docker >/dev/null 2>&1 && echo @@GROUP_DOCKER
{ getent passwd 2>/dev/null || cat /etc/passwd; } | while IFS=: read -r u _ uid _ _ home shell; do
  printf '@@USER\t%s\t%s\t%s\t%s\n' "$u" "$uid" "$home" "$shell"
  for p in $AKF; do
    [ "$p" = none ] && continue
    f=$(printf '%s' "$p" | sed -e "s#%h#$home#g" -e "s#%u#$u#g" -e 's#%%#%#g')
    case $f in /*) ;; *) f="$home/$f" ;; esac
    [ -f "$f" ] || continue
    s=$(wc -c < "$f" 2>/dev/null) || { printf '@@FILE\t%s\t%s\t-1\t0\n' "$u" "$f"; continue; }
    printf '@@FILE\t%s\t%s\t%s\t%s\n' "$u" "$f" "$s" "$(stat -c %Y -- "$f" 2>/dev/null)"
    [ "$s" -le ` + "262144" + ` ] && cat -- "$f"
  done
done`

// ReadKeys lists the users and their authorized SSH keys.
func ReadKeys(ctx context.Context, h Host) (Keys, error) {
	out, stderr, code, err := run(ctx, h, wrap(keysBody), nil, 60*time.Second)
	if err != nil {
		return Keys{}, err
	}
	if code != 0 {
		return Keys{}, failure(out, stderr, code, "Reading the SSH keys")
	}
	return parseKeys(out), nil
}

func parseKeys(out string) Keys {
	me, rest := parseMe(out)
	k := Keys{Root: me.Root, User: me.User, Patterns: []string{}, Users: []KeyUser{}, Files: []KeyFile{}}
	r := bufio.NewReader(strings.NewReader(rest))
	seen := map[string]bool{}
	for {
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		f := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		switch f[0] {
		case "@@GROUP_DOCKER":
			k.Docker = true
		case "@@AKF":
			if len(f) > 1 {
				k.Patterns = strings.Fields(f[1])
			}
		case "@@USER":
			if len(f) < 5 {
				continue
			}
			uid, _ := strconv.Atoi(f[2])
			shell := f[4]
			k.Users = append(k.Users, KeyUser{Name: f[1], UID: uid, Home: f[3], Shell: shell,
				CanLogin: shell != "" && !strings.HasSuffix(shell, "/nologin") && !strings.HasSuffix(shell, "/false") && shell != "/bin/sync"})
		case "@@FILE":
			if len(f) < 5 {
				continue
			}
			size, _ := strconv.ParseInt(f[3], 10, 64)
			mt, _ := strconv.ParseInt(f[4], 10, 64)
			kf := KeyFile{User: f[1], Path: f[2], ModTime: mt * 1000, Readable: size >= 0, TooBig: size > maxKeysFile, Keys: []AuthKey{}}
			if kf.Readable && !kf.TooBig {
				kf.content = make([]byte, size)
				n, _ := io.ReadFull(r, kf.content)
				kf.content = kf.content[:n]
				kf.Keys = parseAuthorizedKeys(kf.content)
			}
			id := kf.User + "\x00" + kf.Path
			if !seen[id] {
				seen[id] = true
				k.Files = append(k.Files, kf)
			}
		}
	}
	return k
}

func parseAuthorizedKeys(b []byte) []AuthKey {
	out := []AuthKey{}
	for i, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		key, ok := parseKeyLine(l)
		key.Line = i + 1
		if !ok {
			cut := l
			if len(cut) > 60 {
				cut = cut[:60] + "…"
			}
			key.Invalid = cut
		}
		out = append(out, key)
	}
	return out
}

func parseKeyLine(l string) (AuthKey, bool) {
	pub, comment, opts, _, err := ssh.ParseAuthorizedKey([]byte(l))
	if err != nil {
		return AuthKey{Options: []string{}}, false
	}
	k := AuthKey{Type: pub.Type(), Fingerprint: ssh.FingerprintSHA256(pub), Comment: comment, Options: opts}
	if k.Options == nil {
		k.Options = []string{}
	}
	if cp, ok := pub.(ssh.CryptoPublicKey); ok {
		if rk, ok := cp.CryptoPublicKey().(*rsa.PublicKey); ok {
			k.Bits = rk.N.BitLen()
			if k.Bits < 2048 {
				k.Weak = fmt.Sprintf("RSA %d bits: too short, use 3072 or more (or Ed25519)", k.Bits)
			}
		}
	}
	if k.Type == ssh.KeyAlgoDSA {
		k.Weak = "DSA keys are no longer accepted by recent OpenSSH"
	}
	return k, true
}

func (kf KeyFile) hash() string {
	if !kf.Readable {
		return ""
	}
	s := sha256.Sum256(kf.content)
	return hex.EncodeToString(s[:])
}

// expand turns an AuthorizedKeysFile pattern into a path for a user.
func expand(pattern string, u KeyUser) string {
	p := strings.NewReplacer("%h", u.Home, "%u", u.Name, "%%", "%").Replace(pattern)
	if !strings.HasPrefix(p, "/") {
		p = strings.TrimSuffix(u.Home, "/") + "/" + p
	}
	return p
}

// writeBody replaces a keys file with stdin, if it still has the content read before (sha256, or "none"
// when it did not exist), keeping its owner and mode. A missing ~/.ssh is created for the user.
const writeBody = `export LC_ALL=C PATH="$PATH:/usr/sbin:/sbin"
u=$4; home=$5; f=$6; want=$7
if [ -e "$f" ]; then
  [ "$(sha256sum < "$f" | cut -d' ' -f1)" = "$want" ] || { echo CHANGED >&2; exit 3; }
else
  [ "$want" = none ] || { echo CHANGED >&2; exit 3; }
  d=$(dirname -- "$f")
  if [ ! -d "$d" ]; then
    mkdir -p -- "$d" || exit 1
    case $d in "$home"/*) chown -- "$u": "$d" && chmod 700 -- "$d" || exit 1 ;; esac
  fi
fi
t=$(mktemp "$f.XXXXXX") || exit 1
if ! cat > "$t"; then rm -f -- "$t"; exit 1; fi
if [ -e "$f" ]; then chown --reference="$f" -- "$t" && chmod --reference="$f" -- "$t"
else chown -- "$u": "$t" && chmod 600 -- "$t"; fi || { rm -f -- "$t"; exit 1; }
mv -f -- "$t" "$f"`

func writeKeys(ctx context.Context, h Host, u KeyUser, path, want string, content []byte) error {
	if want == "" {
		want = "none"
	}
	out, stderr, code, err := run(ctx, h, wrap(writeBody, u.Name, u.Home, path, want), content, 30*time.Second)
	if err != nil {
		return err
	}
	if code == 3 {
		return errors.New("the file was changed on the server meanwhile: refresh and try again")
	}
	if code != 0 {
		return failure(out, stderr, code, "Saving "+path)
	}
	return nil
}

func findUser(k Keys, name string) (KeyUser, bool) {
	for _, u := range k.Users {
		if u.Name == name {
			return u, true
		}
	}
	return KeyUser{}, false
}

var userNameRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,63}\$?$`)

var newUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// createUserBody creates a user who signs in with keys only: home folder, bash, and a password that can
// never match ("*", not "!": sshd without PAM treats "!" as a locked account and refuses its keys too).
const createUserBody = `export LC_ALL=C PATH="$PATH:/usr/sbin:/sbin"
[ "$1" = 2 ] && { echo "You need to be root to run this script" >&2; exit 1; }
u=$4; docker=$5; sudo=$6
if id -- "$u" >/dev/null 2>&1; then echo "EXISTS" >&2; exit 5; fi
useradd -m -s /bin/bash -- "$u" || exit 1
usermod -p '*' -- "$u" || exit 1
if [ "$docker" = 1 ]; then usermod -aG docker -- "$u" || exit 1; fi
if [ "$sudo" = 1 ]; then
  f=/etc/sudoers.d/90-serverdash-$u
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$u" > "$f.tmp" && chmod 440 "$f.tmp" &&
    visudo -cf "$f.tmp" >/dev/null && mv -f "$f.tmp" "$f" || { rm -f "$f.tmp"; echo "cannot write the sudo rule" >&2; exit 1; }
fi`

// CreateUser adds a user who signs in with SSH keys only.
func CreateUser(ctx context.Context, h Host, name string, nu NewUser) error {
	if !newUserRe.MatchString(name) {
		return errors.New("the user name must start with a letter and use only lowercase letters, digits, - and _ (32 at most)")
	}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	out, stderr, code, err := run(ctx, h, wrap(createUserBody, name, flag(nu.Docker), flag(nu.Sudo)), nil, 60*time.Second)
	if err != nil {
		return err
	}
	if code == 5 {
		return fmt.Errorf("the user %s already exists: choose it in the list instead", name)
	}
	if code != 0 {
		return failure(out, stderr, code, "Creating the user")
	}
	return nil
}

// CheckKey validates one public key line.
func CheckKey(line string) (AuthKey, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, "\n\r") {
		return AuthKey{}, errors.New("paste one public key (a single line such as ssh-ed25519 AAAA… name)")
	}
	if strings.Contains(line, "PRIVATE KEY") {
		return AuthKey{}, errors.New("this is a private key: paste the public key (the .pub file) instead")
	}
	key, ok := parseKeyLine(line)
	if !ok {
		return AuthKey{}, errors.New("this is not a valid SSH public key")
	}
	return key, nil
}

// AddKey appends a public key to the user's first authorized_keys file (created when missing).
func AddKey(ctx context.Context, h Host, user, line string) (AuthKey, error) {
	line = strings.TrimSpace(line)
	if !userNameRe.MatchString(user) {
		return AuthKey{}, errors.New("invalid user name")
	}
	key, err := CheckKey(line)
	if err != nil {
		return key, err
	}
	k, err := ReadKeys(ctx, h)
	if err != nil {
		return key, err
	}
	u, ok := findUser(k, user)
	if !ok {
		return key, fmt.Errorf("there is no user %s on this server", user)
	}
	patterns := k.Patterns
	if len(patterns) == 0 || patterns[0] == "none" {
		patterns = []string{".ssh/authorized_keys"}
	}
	path := expand(patterns[0], u)
	var cur *KeyFile
	for i := range k.Files {
		f := &k.Files[i]
		if f.User == user {
			for _, x := range f.Keys {
				if x.Fingerprint == key.Fingerprint {
					return key, fmt.Errorf("this key is already authorized for %s (%s)", user, f.Path)
				}
			}
			if f.Path == path {
				cur = f
			}
		}
	}
	var content []byte
	want := ""
	if cur != nil {
		if !cur.Readable || cur.TooBig {
			return key, fmt.Errorf("cannot read %s: connect as root, or allow sudo without a password", path)
		}
		content, want = append([]byte{}, cur.content...), cur.hash()
		if len(content) > 0 && content[len(content)-1] != '\n' {
			content = append(content, '\n')
		}
	}
	content = append(content, []byte(line+"\n")...)
	return key, writeKeys(ctx, h, u, path, want, content)
}

// RemoveKey removes the lines of a key (by fingerprint) from one of the user's authorized_keys files.
func RemoveKey(ctx context.Context, h Host, user, path, fingerprint string) error {
	k, err := ReadKeys(ctx, h)
	if err != nil {
		return err
	}
	u, ok := findUser(k, user)
	if !ok {
		return fmt.Errorf("there is no user %s on this server", user)
	}
	for _, f := range k.Files {
		if f.User != user || f.Path != path {
			continue
		}
		if !f.Readable || f.TooBig {
			return fmt.Errorf("cannot read %s: connect as root, or allow sudo without a password", path)
		}
		lines := strings.SplitAfter(string(f.content), "\n")
		var keep strings.Builder
		removed := 0
		for _, l := range lines {
			t := strings.TrimSpace(l)
			if t != "" && !strings.HasPrefix(t, "#") {
				if key, ok := parseKeyLine(t); ok && key.Fingerprint == fingerprint {
					removed++
					continue
				}
			}
			keep.WriteString(l)
		}
		if removed == 0 {
			return errors.New("this key is no longer in the file: refresh the list")
		}
		return writeKeys(ctx, h, u, path, f.hash(), []byte(keep.String()))
	}
	return errors.New("this file is no longer on the server: refresh the list")
}
