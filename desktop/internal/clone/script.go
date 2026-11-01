package clone

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"serverdash/internal/docker"
	"serverdash/internal/ops"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

// Image modes (Options.Images): how the target gets each image, before any container is created.
const (
	ImagesAuto  = "auto"  // pull it; when that fails (no docker login), copy it from the source server
	ImagesCopy  = "copy"  // copy it from the source server (unless the target has that exact image)
	ImagesPull  = "pull"  // pull it from its registry
	ImagesLocal = "local" // use the one the target already has
)

type Options struct {
	Images  string `json:"images"`
	Replace bool   `json:"replace"` // remove a target container with the same name; otherwise it is skipped
}

// Plan is what the UI sends to start: the edited containers and the networks they need.
type Plan struct {
	Source   string    `json:"source"`   // the source server's name, for the script's header
	SourceID string    `json:"sourceId"` // to copy images from it
	Specs    []Spec    `json:"specs"`
	Networks []Network `json:"networks"`
	Files    []File    `json:"files"` // .env files written on the target before the containers are created
	Options
}

var (
	// docker --env-file keys: no "=", no whitespace, not a comment.
	envKeyRe  = regexp.MustCompile(`^[^=\s#][^=\s]*$`)
	netNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	imageRe   = regexp.MustCompile(`^[a-zA-Z0-9][^\s]{0,511}$`)
	portRe    = regexp.MustCompile(`^\d{1,5}(-\d{1,5})?$`)
	protoRe   = regexp.MustCompile(`^(tcp|udp|sctp)$`)
	modeRe    = regexp.MustCompile(`^[0-7]{3,4}$`)
)

// docker --env-file reads one line per variable without unquoting: other values go on the command line.
func fitsEnvFile(v string) bool { return !strings.ContainsAny(v, "\n\r") && len(v) < 32*1024 }

func (p Plan) validate() error {
	if len(p.Specs) == 0 {
		return errors.New("no containers to create")
	}
	if len(p.Specs) > maxContainers {
		return fmt.Errorf("at most %d containers at a time", maxContainers)
	}
	seen := map[string]bool{}
	for _, s := range p.Specs {
		if err := docker.ValidateName(s.Name); err != nil {
			return err
		}
		if seen[s.Name] {
			return fmt.Errorf("container %s is listed twice", s.Name)
		}
		seen[s.Name] = true
		if !imageRe.MatchString(s.Image) || strings.HasPrefix(s.Image, "sha256:") {
			return fmt.Errorf("%s: enter a valid image name (like nginx:1.27 or myorg/api:2.3)", s.Name)
		}
		keys := map[string]bool{}
		for _, e := range s.Env {
			if !envKeyRe.MatchString(e.Key) {
				return fmt.Errorf("%s: invalid variable name %q (no spaces or \"=\")", s.Name, e.Key)
			}
			if keys[e.Key] {
				return fmt.Errorf("%s: variable %s is set twice", s.Name, e.Key)
			}
			keys[e.Key] = true
			if strings.ContainsRune(e.Value, 0) {
				return fmt.Errorf("%s: the value of %s contains a NUL character", s.Name, e.Key)
			}
		}
		for _, n := range s.Networks {
			if !netNameRe.MatchString(n.Name) {
				return fmt.Errorf("%s: invalid network name %q", s.Name, n.Name)
			}
			for _, a := range n.Aliases {
				if !netNameRe.MatchString(a) {
					return fmt.Errorf("%s: invalid network alias %q", s.Name, a)
				}
			}
		}
		for _, pt := range s.Ports {
			if err := pt.validate(); err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
		}
		for _, f := range slices.Concat(s.Flags, s.Args) {
			if strings.ContainsRune(f, 0) {
				return fmt.Errorf("%s: an option contains a NUL character", s.Name)
			}
		}
	}
	size := 0
	for _, f := range p.Files {
		if !strings.HasPrefix(f.Path, "/") || strings.ContainsAny(f.Path, "\x00\n\r") || strings.Contains(f.Path, "/../") || configFormat(f.Path) == "" {
			return fmt.Errorf("invalid config file path %q", f.Path)
		}
		if !modeRe.MatchString(f.Mode) || f.UID < 0 || f.GID < 0 {
			return fmt.Errorf("%s: invalid mode or owner", f.Path)
		}
		if size += len(f.Content); size > 4*maxEnvFile {
			return errors.New("the .env files are too big")
		}
	}
	for _, n := range p.Networks {
		if !netNameRe.MatchString(n.Name) || (n.Driver != "" && !netNameRe.MatchString(n.Driver)) {
			return fmt.Errorf("invalid network %q", n.Name)
		}
	}
	return nil
}

// EnvFile returns the docker --env-file content of a container (the variables that fit on one line).
func EnvFile(s Spec) string {
	var b strings.Builder
	for _, e := range s.Env {
		if fitsEnvFile(e.Value) {
			b.WriteString(e.Key + "=" + e.Value + "\n")
		}
	}
	return b.String()
}

// EnvFileName is the env file of the i-th container, next to run.sh.
func EnvFileName(i int) string { return fmt.Sprintf("%d.env", i+1) }

// PlanNetworks lists the user-defined networks the containers need, in the order the script creates them.
func PlanNetworks(p Plan) []Network {
	var nets []Network
	for _, s := range p.Specs {
		for _, a := range s.Networks {
			if slices.ContainsFunc(nets, func(n Network) bool { return n.Name == a.Name }) {
				continue
			}
			n := Network{Name: a.Name, Driver: "bridge"}
			if i := slices.IndexFunc(p.Networks, func(x Network) bool { return x.Name == a.Name }); i >= 0 {
				n = p.Networks[i]
			}
			nets = append(nets, n)
		}
	}
	return nets
}

// ImageRef is an image the containers need: its name, and the ID of the one running on the source (empty when
// the image was changed in the UI: the source may not have it).
type ImageRef struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// PlanImages lists the images of the containers, each once.
func PlanImages(p Plan) []ImageRef {
	var images []ImageRef
	for _, s := range p.Specs {
		i := slices.IndexFunc(images, func(r ImageRef) bool { return r.Name == s.Image })
		if i < 0 {
			images = append(images, ImageRef{Name: s.Image, ID: s.ImageID})
		} else if images[i].ID != s.ImageID {
			images[i].ID = "" // two containers run different builds of one tag: go by the name
		}
	}
	return images
}

// Script builds run.sh: write the .env files, create the networks, pull the images, create and start each
// container (going on after a failure), then check after a few seconds that the started ones keep running.
// Progress goes to the output as event lines "@@<tag> …" (tag is random per run, so no program output can fake
// one); everything else is the log of the step in progress. Between containers it stops when a "cancel" file
// appears next to it. When it ends it deletes the env files and itself; out.log stays for the app to read.
// The script is written to the server as a file, so values never appear in a process list.
func Script(srv store.Server, p Plan, tag string) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	q := ops.Quote
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("#!/bin/sh")
	w("# Written by Server Dashboard: creates %d containers copied from %s.", len(p.Specs), strings.ReplaceAll(p.Source, "\n", " "))
	w(`D=$(cd "$(dirname "$0")" && pwd) || exit 1`)
	w(`trap 'rm -f "$D"/*.env "$D"/*.data "$D/run.sh"' EXIT`)
	w(`trap 'exit 130' HUP INT TERM`)
	w(`DOCKER=%s`, q(sshpool.DockerBin(srv)))
	w(`ev() { echo "@@%s $*"; }`, tag)
	w(`show() { echo "$ $*"; }`)
	w(`stop_if_cancelled() { if [ -f "$D/cancel" ]; then ev cancelled; exit 2; fi; }`)
	w(`failed=0; ok=""`)

	if len(p.Files) > 0 {
		// Written as root when possible (they often live in root's folders); an existing file is kept as a .bak copy.
		w(`ev phase files`)
		w(`S=""; if [ "$(id -u)" -ne 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi`)
		w(`ts=$(date +%%Y%%m%%d-%%H%%M%%S)`)
		w(`writefile() {`)
		w(`  show "write $2"`)
		w(`  $S mkdir -p "$(dirname "$2")" || return 1`)
		w(`  if [ -e "$2" ]; then $S cp -p "$2" "$2.bak-$ts" || return 1; echo "previous file kept as $2.bak-$ts"; fi`)
		w(`  $S install -m "$3" -o "$4" -g "$5" "$1" "$2" 2>/dev/null || $S install -m "$3" "$1" "$2"`)
		w(`}`)
		for i, f := range p.Files {
			w(`ev file %d start`, i)
			w(`if writefile "$D/%s" %s %s %d %d; then ev file %d ok; else ev file %d failed; failed=$((failed+1)); fi`,
				DataFileName(i), q(f.Path), f.Mode, f.UID, f.GID, i, i)
		}
	}

	if nets := PlanNetworks(p); len(nets) > 0 {
		w(`ev phase networks`)
		for i, n := range nets {
			args := []string{"network", "create"}
			if n.Driver != "" && n.Driver != "bridge" {
				// Overlay, macvlan… need more than a name: fall back to a bridge network, said in the log.
				w(`echo %s`, q(fmt.Sprintf("Network %s uses the %s driver on the source: it is created as a bridge network.", n.Name, n.Driver)))
			}
			if n.Internal {
				args = append(args, "--internal")
			}
			args = append(args, n.Name)
			w(`ev net %d start`, i)
			w(`if $DOCKER network inspect %s >/dev/null 2>&1; then ev net %d exists; else show %s; if $DOCKER %s >/dev/null; then ev net %d ok; else ev net %d failed; failed=$((failed+1)); fi; fi`,
				q(n.Name), i, q("docker "+strings.Join(args, " ")), quoteAll(args), i, i)
		}
	}

	w(`ev phase containers`)
	for i, s := range p.Specs {
		fn := fmt.Sprintf("c%d", i+1)
		w(`%s() {`, fn)
		w(`  if $DOCKER container inspect %s >/dev/null 2>&1; then`, q(s.Name))
		if p.Replace {
			w(`    ev c %d step removing`, i)
			w(`    show %s`, q("docker rm -f "+s.Name))
			w(`    $DOCKER rm -f %s >/dev/null || return 1`, q(s.Name))
		} else {
			w(`    echo %s`, q("A container named "+s.Name+" already exists on this server: left as it is."))
			w(`    ev c %d skipped; return 0`, i)
		}
		w(`  fi`)

		args := []string{"--name", s.Name}
		shown := []string{"create", "--name", s.Name}
		if EnvFile(s) != "" {
			shown = append(shown, "--env-file", EnvFileName(i))
		}
		for _, e := range s.Env {
			if !fitsEnvFile(e.Value) {
				args = append(args, "--env", e.Key+"="+e.Value)
				shown = append(shown, "--env", e.Key+"=…")
			}
		}
		var tail []string
		if !s.Bridge && len(s.Networks) > 0 {
			tail = append(tail, "--network", s.Networks[0].Name)
			for _, a := range s.Networks[0].Aliases {
				tail = append(tail, "--network-alias", a)
			}
		}
		for _, pt := range s.Ports {
			tail = append(tail, "--publish", pt.arg())
		}
		tail = append(tail, s.Flags...)
		tail = append(tail, s.Image)
		tail = append(tail, s.Args...)
		args = append(args, tail...)
		shown = append(shown, tail...)
		envFile := ""
		if EnvFile(s) != "" {
			envFile = fmt.Sprintf(` --env-file "$D/%s"`, EnvFileName(i))
		}
		w(`  ev c %d step creating`, i)
		w(`  show %s`, q("docker "+strings.Join(shown, " ")))
		w(`  $DOCKER create%s %s >/dev/null || return 1`, envFile, quoteAll(args))

		extra := s.Networks
		if !s.Bridge && len(extra) > 0 {
			extra = extra[1:]
		}
		for _, n := range extra {
			c := []string{"network", "connect"}
			for _, a := range n.Aliases {
				c = append(c, "--alias", a)
			}
			c = append(c, n.Name, s.Name)
			w(`  show %s`, q("docker "+strings.Join(c, " ")))
			w(`  $DOCKER %s || return 1`, quoteAll(c))
		}
		w(`  ev c %d step starting`, i)
		w(`  $DOCKER start %s >/dev/null || return 1`, q(s.Name))
		w(`  ev c %d started; ok="$ok %d"`, i, i)
		w(`}`)
		w(`stop_if_cancelled`)
		w(`ev c %d start`, i)
		w(`%s || { ev c %d failed; failed=$((failed+1)); }`, fn, i)
	}

	// A container that exits or restarts right after starting usually has a wrong setting: say so, with its log.
	w(`if [ -n "$ok" ]; then`)
	w(`  ev phase verify %d`, verifyWait)
	w(`  sleep %d`, verifyWait)
	w(`  for i in $ok; do`)
	w(`    case $i in`)
	for i, s := range p.Specs {
		w(`    %d) n=%s ;;`, i, q(s.Name))
	}
	w(`    esac`)
	w(`    st=$($DOCKER inspect -f '{{.State.Status}} {{.State.ExitCode}} {{.RestartCount}}' "$n" 2>/dev/null || echo "missing 0 0")`)
	w(`    ev state $i $st`)
	w(`    case "$st" in "running 0 0") ;; *) echo "Last lines of its log:"; $DOCKER logs --tail 30 "$n" 2>&1 | sed 's/^/| /' ;; esac`)
	w(`  done`)
	w(`fi`)
	w(`ev done $failed`)
	w(`[ "$failed" -eq 0 ]`)
	return b.String(), nil
}

// verifyWait is how long the started containers get before their state is checked, in seconds.
var verifyWait = 10

// DataFileName holds the content of the i-th .env file of the plan, next to run.sh.
func DataFileName(i int) string { return fmt.Sprintf("file%d.data", i+1) }

func (p Port) validate() error {
	c, proto, _ := strings.Cut(p.Port, "/")
	if !portRe.MatchString(c) || !inRange(c) || (proto != "" && !protoRe.MatchString(proto)) {
		return fmt.Errorf("invalid container port %q (like 8080/tcp)", p.Port)
	}
	if p.HostPort != "" && (!portRe.MatchString(p.HostPort) || !inRange(p.HostPort)) {
		return fmt.Errorf("invalid host port %q", p.HostPort)
	}
	if p.HostIP != "" && net.ParseIP(p.HostIP) == nil {
		return fmt.Errorf("invalid host IP %q", p.HostIP)
	}
	return nil
}

func inRange(ports string) bool {
	for _, x := range strings.Split(ports, "-") {
		if n, err := strconv.Atoi(x); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

// arg is the --publish value: [ip:][hostPort:]port/proto (an empty host port lets Docker pick one).
func (p Port) arg() string {
	ip := p.HostIP
	if ip == "0.0.0.0" || ip == "::" {
		ip = ""
	}
	switch {
	case ip != "":
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		return ip + ":" + p.HostPort + ":" + p.Port
	case p.HostPort != "":
		return p.HostPort + ":" + p.Port
	}
	return p.Port
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// registryRe: a registry host, with an optional port (empty means Docker Hub).
var registryRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?(:\d{1,5})?)?$`)

// LoginScript runs docker login to a registry (Docker Hub when empty) in a terminal, as the user docker runs as.
func LoginScript(srv store.Server, registry string) (string, error) {
	if !registryRe.MatchString(registry) {
		return "", errors.New("invalid registry")
	}
	hint := `printf 'Log in to Docker Hub with your user name and an access token\n(hub.docker.com → Account settings → Personal access tokens).\n\n'`
	if registry != "" {
		hint = `printf 'Log in to ` + registry + ` (for a GitLab registry: a deploy token with read_registry,\nor your user name and a personal access token).\n\n'`
	}
	return hint + `
` + sshpool.DockerBin(srv) + ` login ` + registry + `
rc=$?
echo
if [ $rc -eq 0 ]; then printf '\033[32m✔ Logged in: go back to the Clone tab and click Check again\033[0m\n'; else printf '\033[31m✘ Login failed (exit %s)\033[0m\n' "$rc"; fi
exit $rc`, nil
}
