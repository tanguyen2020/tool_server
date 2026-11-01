package clone

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"serverdash/internal/ops"
	"serverdash/internal/sshpool"
)

// How a .env file reaches its containers (File.Usage):
//   - mount:    mounted as a file (-v /srv/app/.env:/app/.env): the app reads it
//   - folder:   inside a mounted folder (-v /srv/app:/app): the app reads it
//   - env-file: given to docker run --env-file: Docker keeps only the variables, so the file is found by searching
//     the usual folders for one whose variables all match the container's
//   - manual:   added by its path in the UI
//
// Every one is written to the same path on the target. For env-file and manual ones the UI also sets the
// containers' variables from the file, since that is where Docker took them from.

// Where deploy folders usually are; searched a few levels deep for env files given to --env-file.
var searchRoots = []string{"/home", "/root", "/opt", "/srv", "/data", "/app", "/apps", "/deploy", "/docker", "/var/www"}

const (
	maxSearchFiles = 300
	maxSearchSize  = 64 << 10
)

// Example files hold placeholders, not the values in use.
var exampleRe = regexp.MustCompile(`(?i)(example|sample|template|dist|\.bak|backup|\.old|~)`)

var envLineRe = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=(.*)$`)

// envValues reads a .env file: for each variable, the values a program may have taken from it (docker --env-file
// keeps the text after "=" as it is; dotenv libraries trim it, remove quotes and an inline comment).
func envValues(content string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(content, "\n") {
		m := envLineRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		raw := m[2]
		t := strings.TrimSpace(raw)
		vals := []string{raw, t}
		if len(t) >= 2 && (t[0] == '"' || t[0] == '\'') && t[len(t)-1] == t[0] {
			vals = append(vals, t[1:len(t)-1])
		} else if i := strings.Index(t, " #"); i >= 0 {
			vals = append(vals, strings.TrimSpace(t[:i]))
		}
		out[m[1]] = slices.Compact(vals)
	}
	return out
}

// MatchEnvFile names the containers whose variables include every variable of the file, with the same value.
// A file of a single variable only matches a container that has just that one (a lone TZ=UTC proves nothing).
func MatchEnvFile(content string, specs []Spec) []string {
	vals := envValues(content)
	matched := []string{}
	if len(vals) == 0 {
		return matched
	}
	for _, s := range specs {
		env := map[string]string{}
		for _, e := range s.Env {
			env[e.Key] = e.Value
		}
		if len(vals) == 1 && len(env) != 1 {
			continue
		}
		all := true
		for k, cands := range vals {
			v, ok := env[k]
			if !ok || !slices.Contains(cands, v) {
				all = false
				break
			}
		}
		if all {
			matched = append(matched, s.Name)
		}
	}
	return matched
}

// readFiles reads text files (base64, so the content arrives byte for byte) with their mode and owner, as root
// when possible. Files that are too big, not text or not readable are left out.
func readFiles(ctx context.Context, conn *sshpool.Conn, paths []string, limit int) (map[string]File, error) {
	out := map[string]File{}
	if len(paths) == 0 {
		return out, nil
	}
	var sb strings.Builder
	sb.WriteString(`S=""; if [ "$(id -u)" -ne 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi; `)
	for i, p := range paths {
		q := ops.Quote(p)
		fmt.Fprintf(&sb, `if $S test -f %[1]s && st=$($S stat -c '%%a %%u %%g %%s' -- %[1]s 2>/dev/null); then echo "@@f %d $st"; `, q, i)
		fmt.Fprintf(&sb, `if [ "${st##* }" -le %d ]; then echo "@@c %d $($S base64 -w0 -- %s)"; fi; fi; `, limit, i, q)
	}
	sb.WriteString("true")
	res, err := run(ctx, conn, sb.String(), "reading .env files", 60*time.Second)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(paths))
	read := map[int]bool{}
	for _, line := range strings.Split(res, "\n") {
		f := strings.Fields(strings.TrimRight(line, "\r"))
		if len(f) < 2 {
			continue
		}
		i, err := strconv.Atoi(f[1])
		if err != nil || i < 0 || i >= len(paths) {
			continue
		}
		switch {
		case f[0] == "@@f" && len(f) == 6:
			files[i].Mode = f[2]
			files[i].UID, _ = strconv.Atoi(f[3])
			files[i].GID, _ = strconv.Atoi(f[4])
		case f[0] == "@@c":
			raw := ""
			if len(f) > 2 {
				raw = f[2]
			}
			b, err := base64.StdEncoding.DecodeString(raw)
			if err == nil && utf8.Valid(b) {
				files[i].Content = string(b)
				read[i] = true
			}
		}
	}
	for i, p := range paths {
		if read[i] && modeRe.MatchString(files[i].Mode) {
			f := files[i]
			f.Path = p
			out[p] = f
		}
	}
	return out, nil
}

// readEnvFiles finds the .env files of the containers: mounted ones, ones inside mounted folders, and ones
// given to --env-file (found by their content).
func readEnvFiles(ctx context.Context, conn *sshpool.Conn, specs []Spec, kinds map[string]string) ([]File, error) {
	files := []File{}
	add := func(path, usage, container string) {
		if i := slices.IndexFunc(files, func(f File) bool { return f.Path == path }); i >= 0 {
			if !slices.Contains(files[i].Containers, container) {
				files[i].Containers = append(files[i].Containers, container)
			}
			return
		}
		files = append(files, File{Path: path, Usage: usage, Format: configFormat(path), Containers: []string{container}})
	}
	var dirs []string
	for _, s := range specs {
		for _, b := range s.Binds {
			switch {
			case kinds[b.Source] == "file" && configFormat(b.Source) != "":
				add(b.Source, "mount", s.Name)
			case kinds[b.Source] == "dir" && !slices.Contains(dirs, b.Source) && b.Source != "/":
				dirs = append(dirs, b.Source)
			}
		}
	}

	// One command lists the .env files of the mounted folders and of the usual deploy folders.
	var sb strings.Builder
	sb.WriteString(`S=""; if [ "$(id -u)" -ne 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi; `)
	sb.WriteString(`T=""; command -v timeout >/dev/null 2>&1 && T="timeout 20"; `)
	names := `\( -name '.env' -o -name '.env.*' -o -name '.env-*' -o -name '*.env' \)`
	for i, d := range dirs {
		fmt.Fprintf(&sb, `$S find %s -maxdepth 1 -type f %s -size -%dk 2>/dev/null | sed 's/^/@@d %d /'; `, ops.Quote(d), names, maxEnvFile>>10, i)
	}
	fmt.Fprintf(&sb, `$S $T find %s -maxdepth 5 \( -name node_modules -o -name .git -o -name vendor -o -name .cache -o -name venv \) -prune -o -type f %s -size -%dk -print 2>/dev/null | head -n %d | sed 's/^/@@s /'; `,
		quoteAll(searchRoots), names, maxSearchSize>>10, maxSearchFiles)
	sb.WriteString("true")
	out, err := run(ctx, conn, sb.String(), "looking for .env files", 45*time.Second)
	if err != nil {
		return files, nil // the search is a help: without it the mounted files are still read
	}
	var searched []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "@@d "):
			f := strings.SplitN(line[4:], " ", 2)
			i, err := strconv.Atoi(f[0])
			if err != nil || i < 0 || i >= len(dirs) || len(f) < 2 || exampleRe.MatchString(f[1]) {
				continue
			}
			for _, s := range specs {
				if slices.ContainsFunc(s.Binds, func(b Bind) bool { return b.Source == dirs[i] }) {
					add(f[1], "folder", s.Name)
				}
			}
		case strings.HasPrefix(line, "@@s "):
			if p := line[4:]; !exampleRe.MatchString(p) && !slices.Contains(searched, p) {
				searched = append(searched, p)
			}
		}
	}

	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	found, err := readFiles(ctx, conn, paths, maxEnvFile)
	if err != nil {
		return nil, err
	}
	kept := []File{}
	for _, f := range files {
		if r, ok := found[f.Path]; ok {
			r.Usage, r.Format, r.Containers = f.Usage, f.Format, f.Containers
			kept = append(kept, r)
		}
	}
	searched = slices.DeleteFunc(searched, func(p string) bool { _, ok := found[p]; return ok })
	var matched []File
	if len(searched) > 0 {
		if cands, err := readFiles(ctx, conn, searched, maxSearchSize); err == nil {
			for _, p := range searched {
				r, ok := cands[p]
				if !ok {
					continue
				}
				if r.Containers = MatchEnvFile(r.Content, specs); len(r.Containers) > 0 {
					r.Usage, r.Format = "env-file", "env"
					matched = append(matched, r)
				}
			}
		}
	}
	return append(kept, ownEnvFiles(matched, specs)...), nil
}

// ownEnvFiles keeps, for each container, the env files that are its own when several match it: services sharing
// one $ENV leave identical files in every service folder, and only the one in the container's folder is its own.
// A container's folders are the ones its mounts are in, and any folder named after it. When none of the matching
// files is in such a folder, all of them stay (the user picks).
func ownEnvFiles(files []File, specs []Spec) []File {
	keep := map[string]map[string]bool{} // path -> containers
	for _, s := range specs {
		var cands []File
		for _, f := range files {
			if slices.Contains(f.Containers, s.Name) {
				cands = append(cands, f)
			}
		}
		own := func(f File) bool {
			dir := path.Dir(f.Path)
			if path.Base(dir) == s.Name || strings.HasSuffix(path.Base(dir), "-"+s.Name) || strings.HasSuffix(s.Name, "-"+path.Base(dir)) {
				return true
			}
			for _, b := range s.Binds {
				if b.Source == dir || path.Dir(b.Source) == dir {
					return true
				}
			}
			return false
		}
		if len(cands) > 1 && slices.ContainsFunc(cands, own) {
			cands = slices.DeleteFunc(cands, func(f File) bool { return !own(f) })
		}
		for _, f := range cands {
			if keep[f.Path] == nil {
				keep[f.Path] = map[string]bool{}
			}
			keep[f.Path][s.Name] = true
		}
	}
	out := []File{}
	for _, f := range files {
		if len(keep[f.Path]) == 0 {
			continue
		}
		f.Containers = slices.DeleteFunc(slices.Clone(f.Containers), func(c string) bool { return !keep[f.Path][c] })
		out = append(out, f)
	}
	return out
}

// ReadEnvFile reads a .env file chosen by its path, and names the containers whose variables it holds.
func ReadEnvFile(ctx context.Context, conn *sshpool.Conn, path string, specs []Spec) (File, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\n\r") {
		return File{}, errors.New("enter the full path of the file, like /home/deploy/orders/.env")
	}
	format := configFormat(path)
	if format == "" {
		return File{}, errors.New("choose a .env file (.env, .env.something, .env-something, something.env) or a .json file, outside system folders like /etc")
	}
	found, err := readFiles(ctx, conn, []string{path}, maxEnvFile)
	if err != nil {
		return File{}, err
	}
	f, ok := found[path]
	if !ok {
		return File{}, fmt.Errorf("cannot read %s on the source server (missing, not text, bigger than %d KB, or no permission)", path, maxEnvFile>>10)
	}
	f.Usage, f.Format = "manual", format
	if format == "env" {
		f.Containers = MatchEnvFile(f.Content, specs)
	} else {
		f.Containers = mountedBy(path, specs)
	}
	return f, nil
}

// mountedBy names the containers that mount a file, or the folder it is in.
func mountedBy(path string, specs []Spec) []string {
	out := []string{}
	for _, s := range specs {
		if slices.ContainsFunc(s.Binds, func(b Bind) bool {
			return b.Source == path || strings.HasPrefix(path, strings.TrimSuffix(b.Source, "/")+"/")
		}) {
			out = append(out, s.Name)
		}
	}
	return out
}
