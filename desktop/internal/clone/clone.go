// Package clone copies containers started with docker run from one server to another. It reads their
// settings with docker inspect, checks the target server, and builds the script that recreates them there
// with edited environment variables. Every value ends up in the script as one quoted shell word.
package clone

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"serverdash/internal/docker"
	"serverdash/internal/ops"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Port struct {
	HostIP   string `json:"hostIp"`
	HostPort string `json:"hostPort"` // empty: Docker picks a free port
	Port     string `json:"port"`     // in the container, "80/tcp"
}

type Bind struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"` // on the source server: "dir", "file" or "missing"
}

// Attach is a user-defined network the container is connected to.
type Attach struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
}

type Network struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Internal bool   `json:"internal"`
}

// File is a .env file mounted into containers (-v /srv/app/.env:/app/.env): the app reads it itself, so it is
// edited like the variables and written to the same path on the target, with the same mode and owner.
type File struct {
	Path       string   `json:"path"`
	Content    string   `json:"content"`
	Mode       string   `json:"mode"` // octal, like "644"
	UID        int      `json:"uid"`
	GID        int      `json:"gid"`
	Containers []string `json:"containers"`
	Usage      string   `json:"usage"`  // mount, folder, env-file or manual (see envfiles.go)
	Format     string   `json:"format"` // env or json
}

// envFileRe: .env, .env.production, .env-gademy, app.env…
var envFileRe = regexp.MustCompile(`(^|/)(\.env([.-][^/]+)?|[^/]+\.env)$`)

// jsonFileRe: appsettings.json, appsettings.Production.json, config.json…
var jsonFileRe = regexp.MustCompile(`(^|/)[^/]+\.json$`)

// systemDirRe: config files are never written there on the target (a mounted /etc/hosts stays the target's own).
var systemDirRe = regexp.MustCompile(`^/(etc|proc|sys|dev|run|var/run|usr|bin|sbin|lib|lib32|lib64|boot)(/|$)`)

// configFormat says how a file is edited: "env" (KEY=VALUE lines), "json" (values by key path), or "" (not a
// config file the clone edits).
func configFormat(path string) string {
	switch {
	case systemDirRe.MatchString(path):
		return ""
	case envFileRe.MatchString(path):
		return "env"
	case jsonFileRe.MatchString(path):
		return "json"
	}
	return ""
}

const maxEnvFile = 256 << 10

// Spec is everything needed to recreate one container. The UI edits Image and Env; the rest goes back as read.
type Spec struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	ImageID string   `json:"imageId"` // of the image the source runs (to copy that exact one)
	Env     []EnvVar `json:"env"`
	Running bool     `json:"running"` // on the source server
	// Flags are the other docker create options (ports, volumes, restart policy…), one argument each.
	Flags []string `json:"flags"`
	Args  []string `json:"args"` // the command after the image, when it differs from the image's
	// Bridge: the container is on Docker's default bridge network too; otherwise it is created on Networks[0].
	Bridge   bool     `json:"bridge"`
	Networks []Attach `json:"networks"`
	Ports    []Port   `json:"ports"`
	Binds    []Bind   `json:"binds"`
	Volumes  []string `json:"volumes"` // named volumes
	Warnings []string `json:"warnings"`
}

// Source is what Read returns: the containers and the facts about the source server the checks need.
type Source struct {
	Specs    []Spec    `json:"specs"`
	Networks []Network `json:"networks"`
	Files    []File    `json:"files"`    // .env files mounted into the containers
	HubLogin bool      `json:"hubLogin"` // logged in to Docker Hub
	Arch     string    `json:"arch"`
}

const maxContainers = 300

func run(ctx context.Context, conn *sshpool.Conn, cmd, what string, timeout time.Duration) (string, error) {
	res, err := conn.Exec(ctx, cmd, timeout)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr + "\n" + res.Stdout)
		if msg == "" {
			msg = fmt.Sprintf("%s failed (exit %d)", what, res.Code)
		}
		return res.Stdout, errors.New(msg)
	}
	return res.Stdout, nil
}

func quoteAll(list []string) string {
	q := make([]string, len(list))
	for i, s := range list {
		q[i] = ops.Quote(s)
	}
	return strings.Join(q, " ")
}

// hubCheck succeeds when the user docker runs as has Docker Hub credentials.
func hubCheck(srv store.Server) string {
	c := `grep -qs 'index.docker.io' "$HOME/.docker/config.json"`
	if srv.UseSudo {
		return "sudo -n sh -c " + ops.Quote(c)
	}
	return c
}

// ------------------------------------------------------------------ docker inspect

type health struct {
	Test          []string
	Interval      int64
	Timeout       int64
	StartPeriod   int64
	StartInterval int64
	Retries       int
}

type inspect struct {
	ID     string `json:"Id"`
	Name   string
	Image  string // the image ID
	State  struct{ Running bool }
	Config struct {
		Hostname    string
		Domainname  string
		User        string
		WorkingDir  string
		Image       string // the name it was started from
		StopSignal  string
		StopTimeout *int
		Env         []string
		Cmd         []string
		Entrypoint  []string
		Labels      map[string]string
		Tty         bool
		OpenStdin   bool
		Healthcheck *health
	}
	HostConfig struct {
		NetworkMode  string
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string
		}
		PublishAllPorts bool
		RestartPolicy   struct {
			Name              string
			MaximumRetryCount int
		}
		Privileged     bool
		ReadonlyRootfs bool
		Init           *bool
		CapAdd         []string
		CapDrop        []string
		DNS            []string `json:"Dns"`
		DNSSearch      []string `json:"DnsSearch"`
		DNSOptions     []string `json:"DnsOptions"`
		ExtraHosts     []string
		SecurityOpt    []string
		GroupAdd       []string
		Links          []string
		Devices        []struct{ PathOnHost, PathInContainer, CgroupPermissions string }
		DeviceRequests []struct {
			Count        int
			DeviceIDs    []string
			Capabilities [][]string
		}
		Sysctls map[string]string
		Ulimits []struct {
			Name       string
			Soft, Hard int64
		}
		Tmpfs             map[string]string
		Memory            int64
		MemoryReservation int64
		MemorySwap        int64
		NanoCpus          int64
		CPUShares         int64  `json:"CpuShares"`
		CpusetCpus        string `json:"CpusetCpus"`
		ShmSize           int64
		PidMode           string
		IpcMode           string
		UTSMode           string
		Runtime           string
		LogConfig         struct {
			Type   string
			Config map[string]string
		}
	}
	Mounts []struct {
		Type, Name, Source, Destination, Driver, Mode, Propagation string
		RW                                                         bool
	}
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases    []string
			IPAMConfig *struct {
				IPv4Address string
				IPv6Address string
			}
		}
	}
}

type imageConfig struct {
	Env         []string
	Cmd         []string
	Entrypoint  []string
	Labels      map[string]string
	User        string
	WorkingDir  string
	StopSignal  string
	Healthcheck *health
	Volumes     map[string]struct{}
}

type imageInspect struct {
	ID     string `json:"Id"`
	Config imageConfig
}

var anonVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// logDrivers are built into Docker, so they exist on the target too.
var logDrivers = []string{"json-file", "local", "journald", "syslog", "none"}

const defaultShm = 64 << 20

// build turns one container into a Spec. byID names the other copied containers (for --network container:).
func build(c inspect, img imageConfig, byID map[string]string) Spec {
	s := Spec{Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image, ImageID: c.Image, Running: c.State.Running, Env: []EnvVar{}}
	flag := func(f ...string) { s.Flags = append(s.Flags, f...) }
	warn := func(format string, a ...any) { s.Warnings = append(s.Warnings, fmt.Sprintf(format, a...)) }
	hc := c.HostConfig

	if strings.HasPrefix(s.Image, "sha256:") {
		warn("It was started from an image ID, not a name: enter the image name so the target can pull it.")
	}

	// Only the variables set for the container, not the ones the image already sets.
	fromImage := map[string]bool{}
	for _, kv := range img.Env {
		fromImage[kv] = true
	}
	for _, kv := range c.Config.Env {
		if fromImage[kv] {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		s.Env = append(s.Env, EnvVar{Key: k, Value: v})
	}

	mode := hc.NetworkMode
	shared := strings.HasPrefix(mode, "container:")
	if c.Config.Hostname != "" && len(c.ID) >= 12 && c.Config.Hostname != c.ID[:12] && mode != "host" && !shared {
		flag("--hostname", c.Config.Hostname)
	}
	if c.Config.Domainname != "" {
		flag("--domainname", c.Config.Domainname)
	}
	if c.Config.User != img.User && c.Config.User != "" {
		flag("--user", c.Config.User)
	}
	if c.Config.WorkingDir != img.WorkingDir && c.Config.WorkingDir != "" {
		flag("--workdir", c.Config.WorkingDir)
	}
	if c.Config.Tty {
		flag("--tty")
	}
	if c.Config.OpenStdin {
		flag("--interactive")
	}

	// Entrypoint and command: a changed entrypoint drops the image's command, so the full command is given.
	if !slices.Equal(c.Config.Entrypoint, img.Entrypoint) {
		if len(c.Config.Entrypoint) == 0 {
			flag("--entrypoint", "")
			s.Args = slices.Clone(c.Config.Cmd)
		} else {
			flag("--entrypoint", c.Config.Entrypoint[0])
			s.Args = slices.Concat(c.Config.Entrypoint[1:], c.Config.Cmd)
		}
	} else if !slices.Equal(c.Config.Cmd, img.Cmd) {
		s.Args = slices.Clone(c.Config.Cmd)
	}

	// Published ports stay apart from the other options: the UI edits them (Script turns them into --publish).
	s.Ports = []Port{}
	for _, p := range slices.Sorted(maps.Keys(hc.PortBindings)) {
		for _, b := range hc.PortBindings[p] {
			ip := b.HostIP
			if ip == "0.0.0.0" || ip == "::" {
				ip = ""
			}
			s.Ports = append(s.Ports, Port{HostIP: ip, HostPort: b.HostPort, Port: p})
		}
	}
	if hc.PublishAllPorts {
		flag("--publish-all")
	}
	if r := hc.RestartPolicy; r.Name != "" && r.Name != "no" {
		if r.Name == "on-failure" && r.MaximumRetryCount > 0 {
			flag("--restart", fmt.Sprintf("on-failure:%d", r.MaximumRetryCount))
		} else {
			flag("--restart", r.Name)
		}
	}

	for _, m := range c.Mounts {
		var opts []string
		if !m.RW {
			opts = append(opts, "ro")
		}
		suffix := ""
		switch m.Type {
		case "bind":
			// SELinux relabelling; Docker reports "z" on every named volume by itself, so only binds keep it.
			for _, o := range strings.Split(m.Mode, ",") {
				if o == "z" || o == "Z" {
					opts = append(opts, o)
				}
			}
			if m.Propagation != "" && m.Propagation != "rprivate" {
				opts = append(opts, m.Propagation)
			}
			if len(opts) > 0 {
				suffix = ":" + strings.Join(opts, ",")
			}
			if strings.Contains(m.Source, ":") || strings.Contains(m.Destination, ":") {
				warn("Bind mount %s → %s is not copied: its path contains a colon.", m.Source, m.Destination)
				continue
			}
			flag("--volume", m.Source+":"+m.Destination+suffix)
			s.Binds = append(s.Binds, Bind{Source: m.Source, Target: m.Destination})
		case "volume":
			if len(opts) > 0 {
				suffix = ":" + strings.Join(opts, ",")
			}
			if anonVolume.MatchString(m.Name) {
				// An anonymous volume: Docker creates a new one for the image's VOLUME folders by itself.
				if _, ok := img.Volumes[m.Destination]; !ok {
					flag("--volume", m.Destination)
				}
				continue
			}
			if m.Driver != "" && m.Driver != "local" {
				warn("Volume %s uses the %s driver on the source: it is created with the local driver.", m.Name, m.Driver)
			}
			flag("--volume", m.Name+":"+m.Destination+suffix)
			s.Volumes = append(s.Volumes, m.Name)
		case "tmpfs":
			if _, ok := hc.Tmpfs[m.Destination]; !ok {
				flag("--mount", "type=tmpfs,destination="+m.Destination)
			}
		}
	}
	for _, p := range slices.Sorted(maps.Keys(hc.Tmpfs)) {
		v := p
		if o := hc.Tmpfs[p]; o != "" {
			v += ":" + o
		}
		flag("--tmpfs", v)
	}

	// Networks: host/none/another container's as an option; user-defined ones are created and attached by the script.
	switch {
	case mode == "host" || mode == "none":
		flag("--network", mode)
	case shared:
		ref := strings.TrimPrefix(mode, "container:")
		if name, ok := byID[ref]; ok {
			flag("--network", "container:"+name)
		} else {
			warn("It shares the network of container %s, which is not copied: it gets its own network instead.", short(ref))
		}
	default:
		_, s.Bridge = c.NetworkSettings.Networks["bridge"]
		names := slices.Sorted(maps.Keys(c.NetworkSettings.Networks))
		// The network it was created on goes first.
		if i := slices.Index(names, mode); i > 0 {
			names = append([]string{mode}, slices.Delete(names, i, i+1)...)
		}
		for _, n := range names {
			if n == "bridge" || n == "host" || n == "none" {
				continue
			}
			ep := c.NetworkSettings.Networks[n]
			a := Attach{Name: n, Aliases: []string{}}
			for _, al := range ep.Aliases {
				if al != s.Name && (len(c.ID) < 12 || al != c.ID[:12]) && !slices.Contains(a.Aliases, al) {
					a.Aliases = append(a.Aliases, al)
				}
			}
			if ep.IPAMConfig != nil && (ep.IPAMConfig.IPv4Address != "" || ep.IPAMConfig.IPv6Address != "") {
				warn("Its fixed IP address on network %s is not copied: Docker assigns a new one.", n)
			}
			s.Networks = append(s.Networks, a)
		}
	}
	if len(hc.Links) > 0 {
		warn("Legacy --link connections are not copied: put the containers on one user-defined network instead.")
	}

	if hc.Privileged {
		flag("--privileged")
	}
	if hc.ReadonlyRootfs {
		flag("--read-only")
	}
	if hc.Init != nil && *hc.Init {
		flag("--init")
	}
	multi := func(name string, list []string) {
		for _, v := range list {
			flag(name, v)
		}
	}
	multi("--cap-add", hc.CapAdd)
	multi("--cap-drop", hc.CapDrop)
	multi("--dns", hc.DNS)
	multi("--dns-search", hc.DNSSearch)
	multi("--dns-option", hc.DNSOptions)
	multi("--add-host", hc.ExtraHosts)
	multi("--security-opt", hc.SecurityOpt)
	multi("--group-add", hc.GroupAdd)
	for _, d := range hc.Devices {
		v := d.PathOnHost + ":" + d.PathInContainer
		if d.CgroupPermissions != "" && d.CgroupPermissions != "rwm" {
			v += ":" + d.CgroupPermissions
		}
		flag("--device", v)
	}
	for _, r := range hc.DeviceRequests {
		gpu := slices.ContainsFunc(r.Capabilities, func(c []string) bool { return slices.Contains(c, "gpu") })
		switch {
		case !gpu:
			warn("A device request (not a GPU) is not copied.")
		case len(r.DeviceIDs) > 0:
			flag("--gpus", `"device=`+strings.Join(r.DeviceIDs, ",")+`"`)
		case r.Count < 0:
			flag("--gpus", "all")
		default:
			flag("--gpus", strconv.Itoa(r.Count))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(hc.Sysctls)) {
		flag("--sysctl", k+"="+hc.Sysctls[k])
	}
	for _, u := range hc.Ulimits {
		flag("--ulimit", fmt.Sprintf("%s=%d:%d", u.Name, u.Soft, u.Hard))
	}
	if hc.Memory > 0 {
		flag("--memory", strconv.FormatInt(hc.Memory, 10))
	}
	if hc.MemoryReservation > 0 {
		flag("--memory-reservation", strconv.FormatInt(hc.MemoryReservation, 10))
	}
	if hc.MemorySwap != 0 && hc.MemorySwap != 2*hc.Memory {
		flag("--memory-swap", strconv.FormatInt(hc.MemorySwap, 10))
	}
	if hc.NanoCpus > 0 {
		flag("--cpus", strconv.FormatFloat(float64(hc.NanoCpus)/1e9, 'f', -1, 64))
	}
	if hc.CPUShares > 0 {
		flag("--cpu-shares", strconv.FormatInt(hc.CPUShares, 10))
	}
	if hc.CpusetCpus != "" {
		flag("--cpuset-cpus", hc.CpusetCpus)
	}
	if hc.ShmSize > 0 && hc.ShmSize != defaultShm {
		flag("--shm-size", strconv.FormatInt(hc.ShmSize, 10))
	}
	if hc.PidMode == "host" {
		flag("--pid", "host")
	}
	if hc.IpcMode == "host" {
		flag("--ipc", "host")
	}
	if hc.UTSMode == "host" {
		flag("--uts", "host")
	}
	if hc.Runtime != "" && hc.Runtime != "runc" {
		flag("--runtime", hc.Runtime)
	}
	// The log driver is copied only with options (e.g. a size limit): otherwise the target's own default applies.
	if lc := hc.LogConfig; lc.Type != "" && (len(lc.Config) > 0 || lc.Type != "json-file") {
		if slices.Contains(logDrivers, lc.Type) {
			flag("--log-driver", lc.Type)
			for _, k := range slices.Sorted(maps.Keys(lc.Config)) {
				flag("--log-opt", k+"="+lc.Config[k])
			}
		} else {
			warn("The %s log driver is not copied: the target's default log driver is used.", lc.Type)
		}
	}

	if h := c.Config.Healthcheck; h != nil && !reflect.DeepEqual(h, img.Healthcheck) {
		switch {
		case len(h.Test) > 0 && h.Test[0] == "NONE":
			flag("--no-healthcheck")
		case len(h.Test) > 1 && h.Test[0] == "CMD-SHELL":
			flag("--health-cmd", h.Test[1])
		case len(h.Test) > 1 && h.Test[0] == "CMD":
			flag("--health-cmd", quoteAll(h.Test[1:]))
		}
		dur := func(name string, ns int64) {
			if ns > 0 {
				flag(name, time.Duration(ns).String())
			}
		}
		dur("--health-interval", h.Interval)
		dur("--health-timeout", h.Timeout)
		dur("--health-start-period", h.StartPeriod)
		if h.Retries > 0 {
			flag("--health-retries", strconv.Itoa(h.Retries))
		}
	}
	if c.Config.StopSignal != "" && c.Config.StopSignal != img.StopSignal {
		flag("--stop-signal", c.Config.StopSignal)
	}
	if c.Config.StopTimeout != nil {
		flag("--stop-timeout", strconv.Itoa(*c.Config.StopTimeout))
	}
	for _, k := range slices.Sorted(maps.Keys(c.Config.Labels)) {
		v := c.Config.Labels[k]
		if iv, ok := img.Labels[k]; (ok && iv == v) || strings.HasPrefix(k, "com.docker.compose.") {
			continue
		}
		flag("--label", k+"="+v)
	}
	if s.Flags == nil {
		s.Flags = []string{}
	}
	return s
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Read inspects the named containers on the source server.
func Read(ctx context.Context, conn *sshpool.Conn, srv store.Server, names []string) (Source, error) {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	if len(names) == 0 {
		return Source{}, errors.New("choose at least one container")
	}
	if len(names) > maxContainers {
		return Source{}, fmt.Errorf("at most %d containers at a time", maxContainers)
	}
	for _, n := range names {
		if err := docker.ValidateName(n); err != nil {
			return Source{}, err
		}
	}
	dk := sshpool.DockerBin(srv)
	out, err := run(ctx, conn, dk+" inspect --type container -- "+quoteAll(names), "docker inspect", 60*time.Second)
	if err != nil {
		return Source{}, err
	}
	var list []inspect
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return Source{}, fmt.Errorf("cannot read docker inspect: %w", err)
	}

	images := map[string]imageConfig{}
	var ids []string
	for _, c := range list {
		if c.Image != "" && !slices.Contains(ids, c.Image) {
			ids = append(ids, c.Image)
		}
	}
	imgErr := error(nil)
	if len(ids) > 0 {
		var imgs []imageInspect
		out, err := run(ctx, conn, dk+" image inspect -- "+quoteAll(ids), "docker image inspect", 60*time.Second)
		if err == nil {
			err = json.Unmarshal([]byte(out), &imgs)
		}
		imgErr = err
		for _, im := range imgs {
			images[im.ID] = im.Config
		}
	}

	byID := map[string]string{}
	for _, c := range list {
		byID[c.ID] = strings.TrimPrefix(c.Name, "/")
	}
	src := Source{Specs: make([]Spec, 0, len(list)), Networks: []Network{}}
	var paths, nets []string
	for _, c := range list {
		img, ok := images[c.Image]
		s := build(c, img, byID)
		if !ok && imgErr != nil {
			s.Warnings = append(s.Warnings, "Its image could not be read: every variable is listed, also the ones the image sets.")
		}
		for _, b := range s.Binds {
			if !slices.Contains(paths, b.Source) {
				paths = append(paths, b.Source)
			}
		}
		for _, n := range s.Networks {
			if !slices.Contains(nets, n.Name) {
				nets = append(nets, n.Name)
			}
		}
		src.Specs = append(src.Specs, s)
	}

	// Facts about the source: bind mount kinds, networks, Docker Hub login and CPU architecture.
	var sb strings.Builder
	sb.WriteString(`S=""; if [ "$(id -u)" -ne 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi; `)
	sb.WriteString(`echo "@@arch $(uname -m)"; `)
	sb.WriteString(hubCheck(srv) + ` && echo "@@hub"; `)
	if len(paths) > 0 {
		sb.WriteString("for p in " + quoteAll(paths) + `; do if $S test -d "$p"; then echo "@@dir $p"; elif $S test -e "$p"; then echo "@@file $p"; fi; done; `)
	}
	if len(nets) > 0 {
		sb.WriteString(dk + ` network inspect --format '@@net {{.Name}}|{{.Driver}}|{{.Internal}}' -- ` + quoteAll(nets) + " 2>/dev/null; ")
	}
	sb.WriteString("true")
	facts, err := run(ctx, conn, sb.String(), "checks", 30*time.Second)
	if err != nil {
		return Source{}, err
	}
	kinds := map[string]string{}
	for _, line := range strings.Split(facts, "\n") {
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r"), " ")
		switch tag {
		case "@@arch":
			src.Arch = rest
		case "@@hub":
			src.HubLogin = true
		case "@@dir":
			kinds[rest] = "dir"
		case "@@file":
			kinds[rest] = "file"
		case "@@net":
			f := strings.Split(rest, "|")
			if len(f) == 3 {
				src.Networks = append(src.Networks, Network{Name: f[0], Driver: f[1], Internal: f[2] == "true"})
			}
		}
	}
	for i := range src.Specs {
		for j, b := range src.Specs[i].Binds {
			src.Specs[i].Binds[j].Kind = cmp.Or(kinds[b.Source], "missing")
		}
	}
	for _, n := range nets {
		if !slices.ContainsFunc(src.Networks, func(x Network) bool { return x.Name == n }) {
			src.Networks = append(src.Networks, Network{Name: n, Driver: "bridge"})
		}
	}
	src.Files, err = readEnvFiles(ctx, conn, src.Specs, kinds)
	return src, err
}

// ------------------------------------------------------------------ checks on the target

type Existing struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type PortConflict struct {
	Container string `json:"container"` // the copied container that wants the port
	Port      string `json:"port"`      // "8080/tcp"
	By        string `json:"by"`        // who holds it on the target
	ByName    string `json:"byName"`    // the target container holding it, if one does
}

type MissingPath struct {
	Container string `json:"container"`
	Source    string `json:"source"`
	Kind      string `json:"kind"` // on the source server
}

type Preflight struct {
	Docker   string         `json:"docker"`
	Arch     string         `json:"arch"`
	HubLogin bool           `json:"hubLogin"`
	Existing []Existing     `json:"existing"`
	Ports    []PortConflict `json:"ports"`
	Paths    []MissingPath  `json:"paths"`
	Networks []string       `json:"networks"` // to create
	Volumes  []string       `json:"volumes"`  // named volumes that do not exist yet: they start empty
	Images   []ImageCheck   `json:"images"`
	Files    []string       `json:"files"` // config files to write that already exist on the target
}

// ImageCheck says whether the target already has an image: the exact build the source runs, or another one.
type ImageCheck struct {
	Name     string `json:"name"`
	OnTarget bool   `json:"onTarget"`
	Same     bool   `json:"same"`
}

var publishedRe = regexp.MustCompile(`:(\d+)->\d+/(tcp|udp|sctp)`)

// Check looks at the target server: name clashes, ports in use, missing folders, networks and volumes to create.
// files are the config files the clone writes: the check says which ones the target already has.
func Check(ctx context.Context, conn *sshpool.Conn, srv store.Server, specs []Spec, files []string) (Preflight, error) {
	dk := sshpool.DockerBin(srv)
	var paths []string
	for _, s := range specs {
		for _, b := range s.Binds {
			if !slices.Contains(paths, b.Source) && strings.HasPrefix(b.Source, "/") && !strings.ContainsAny(b.Source, "\x00\n\r") {
				paths = append(paths, b.Source)
			}
		}
	}
	var sb strings.Builder
	sb.WriteString(`S=""; if [ "$(id -u)" -ne 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi; `)
	sb.WriteString(`echo "@@docker $(` + dk + ` version --format '{{.Server.Version}}' 2>/dev/null)"; `)
	sb.WriteString(`echo "@@arch $(uname -m)"; `)
	sb.WriteString(hubCheck(srv) + ` && echo "@@hub"; `)
	sb.WriteString(dk + ` ps -a --format '@@ct {{.Names}}|{{.State}}|{{.Ports}}' 2>/dev/null; `)
	sb.WriteString(dk + ` network ls --format '@@net {{.Name}}' 2>/dev/null; `)
	sb.WriteString(dk + ` volume ls --format '@@vol {{.Name}}' 2>/dev/null; `)
	images := PlanImages(Plan{Specs: specs})
	for i, img := range images {
		fmt.Fprintf(&sb, `%s image inspect --format '@@img %d {{.Id}}' -- %s 2>/dev/null; `, dk, i, ops.Quote(img.Name))
	}
	if len(paths) > 0 {
		sb.WriteString("for p in " + quoteAll(paths) + `; do $S test -e "$p" || echo "@@missing $p"; done; `)
	}
	var toWrite []string
	for _, f := range files {
		if strings.HasPrefix(f, "/") && !strings.ContainsAny(f, "\x00\n\r") {
			toWrite = append(toWrite, f)
		}
	}
	if len(toWrite) > 0 {
		sb.WriteString("for p in " + quoteAll(toWrite) + `; do $S test -e "$p" && echo "@@fexists $p"; done; `)
	}
	sb.WriteString("true")
	out, err := run(ctx, conn, sb.String(), "checks", 60*time.Second)
	if err != nil {
		return Preflight{}, err
	}
	pf := Preflight{Existing: []Existing{}, Ports: []PortConflict{}, Paths: []MissingPath{}, Networks: []string{}, Volumes: []string{}, Images: []ImageCheck{}, Files: []string{}}
	targetIDs := map[int]string{}
	states := map[string]string{}
	held := map[string]string{} // "8080/tcp" -> target container
	nets, vols, missing := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r"), " ")
		switch tag {
		case "@@docker":
			pf.Docker = strings.TrimSpace(rest)
		case "@@arch":
			pf.Arch = rest
		case "@@hub":
			pf.HubLogin = true
		case "@@ct":
			f := strings.SplitN(rest, "|", 3)
			if len(f) < 3 {
				continue
			}
			states[f[0]] = f[1]
			for _, m := range publishedRe.FindAllStringSubmatch(f[2], -1) {
				held[m[1]+"/"+m[2]] = f[0]
			}
		case "@@net":
			nets[rest] = true
		case "@@vol":
			vols[rest] = true
		case "@@missing":
			missing[rest] = true
		case "@@fexists":
			pf.Files = append(pf.Files, rest)
		case "@@img":
			if n, id, ok := strings.Cut(rest, " "); ok {
				if i, err := strconv.Atoi(n); err == nil && i >= 0 && i < len(images) {
					targetIDs[i] = id
				}
			}
		}
	}
	for i, img := range images {
		id := targetIDs[i]
		pf.Images = append(pf.Images, ImageCheck{Name: img.Name, OnTarget: id != "", Same: id != "" && id == img.ID})
	}
	if pf.Docker == "" {
		return Preflight{}, errors.New("Docker does not answer on the target server")
	}
	listening, _, _ := ops.ListeningPorts(ctx, conn)
	taken := map[string]string{} // host ports the copies themselves publish -> container
	for _, s := range specs {
		if st, ok := states[s.Name]; ok {
			pf.Existing = append(pf.Existing, Existing{Name: s.Name, State: st})
		}
		for _, p := range s.Ports {
			hp, err := strconv.Atoi(p.HostPort)
			if err != nil {
				continue
			}
			proto := "tcp"
			if _, after, ok := strings.Cut(p.Port, "/"); ok {
				proto = after
			}
			key := fmt.Sprintf("%d/%s", hp, proto)
			if other, ok := taken[key]; ok {
				pf.Ports = append(pf.Ports, PortConflict{Container: s.Name, Port: key, By: "copied container " + other + " too"})
				continue
			}
			taken[key] = s.Name
			if name, ok := held[key]; ok {
				pf.Ports = append(pf.Ports, PortConflict{Container: s.Name, Port: key, By: "container " + name, ByName: name})
				continue
			}
			for _, l := range listening {
				if l.Port == hp && l.Proto == proto {
					pf.Ports = append(pf.Ports, PortConflict{Container: s.Name, Port: key, By: cmp.Or(l.Process, "another program")})
					break
				}
			}
		}
		for _, b := range s.Binds {
			if missing[b.Source] {
				pf.Paths = append(pf.Paths, MissingPath{Container: s.Name, Source: b.Source, Kind: b.Kind})
			}
		}
		for _, n := range s.Networks {
			if !nets[n.Name] && !slices.Contains(pf.Networks, n.Name) {
				pf.Networks = append(pf.Networks, n.Name)
			}
		}
		for _, v := range s.Volumes {
			if !vols[v] && !slices.Contains(pf.Volumes, v) {
				pf.Volumes = append(pf.Volumes, v)
			}
		}
	}
	return pf, nil
}
