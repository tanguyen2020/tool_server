package clone

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"serverdash/internal/store"
)

const sampleContainer = `{
  "Id": "4f1c2a9b8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928170615243a",
  "Name": "/orders-api",
  "Image": "sha256:img1",
  "State": {"Running": true},
  "Config": {
    "Hostname": "4f1c2a9b8e7d",
    "Image": "myorg/orders-api:2.3",
    "Env": ["DB_HOST=10.0.0.5", "DB_PASSWORD=s3cr'et $x", "PATH=/usr/local/bin:/usr/bin", "JAVA_OPTS=-Xmx512m"],
    "Cmd": ["java", "-jar", "app.jar"],
    "Entrypoint": null,
    "Labels": {"maintainer": "team", "com.docker.compose.project": "x", "tier": "backend"},
    "WorkingDir": "/app"
  },
  "HostConfig": {
    "NetworkMode": "backend",
    "PortBindings": {"8080/tcp": [{"HostIp": "", "HostPort": "8081"}], "9090/tcp": [{"HostIp": "127.0.0.1", "HostPort": "9090"}]},
    "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
    "Memory": 536870912,
    "MemorySwap": 1073741824,
    "NanoCpus": 1500000000,
    "ShmSize": 67108864,
    "ExtraHosts": ["db.local:10.0.0.5"],
    "LogConfig": {"Type": "json-file", "Config": {"max-size": "10m", "max-file": "3"}},
    "Tmpfs": {"/tmp": "rw,size=64m"}
  },
  "Mounts": [
    {"Type": "bind", "Source": "/srv/orders/config.yml", "Destination": "/app/config.yml", "Mode": "ro", "RW": false, "Propagation": "rprivate"},
    {"Type": "volume", "Name": "orders-data", "Source": "/var/lib/docker/volumes/orders-data/_data", "Destination": "/data", "Driver": "local", "Mode": "z", "RW": true},
    {"Type": "volume", "Name": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "Destination": "/cache", "Driver": "local", "RW": true}
  ],
  "NetworkSettings": {"Networks": {
    "backend": {"Aliases": ["orders-api", "4f1c2a9b8e7d", "orders"]},
    "monitoring": {"Aliases": null, "IPAMConfig": {"IPv4Address": "172.30.0.9"}}
  }}
}`

const sampleImage = `{
  "Id": "sha256:img1",
  "Config": {
    "Env": ["PATH=/usr/local/bin:/usr/bin", "JAVA_OPTS=-Xmx256m"],
    "Cmd": ["java", "-jar", "app.jar"],
    "Labels": {"maintainer": "team"},
    "WorkingDir": "/app",
    "Volumes": {"/cache": {}}
  }
}`

func sampleSpec(t *testing.T) Spec {
	t.Helper()
	var c inspect
	var img imageInspect
	if err := json.Unmarshal([]byte(sampleContainer), &c); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(sampleImage), &img); err != nil {
		t.Fatal(err)
	}
	return build(c, img.Config, nil)
}

func TestBuild(t *testing.T) {
	s := sampleSpec(t)
	if s.Name != "orders-api" || s.Image != "myorg/orders-api:2.3" || !s.Running {
		t.Fatalf("name/image/running: %+v", s)
	}
	// PATH comes from the image unchanged: left out. JAVA_OPTS differs from the image's: kept.
	want := []EnvVar{{"DB_HOST", "10.0.0.5"}, {"DB_PASSWORD", "s3cr'et $x"}, {"JAVA_OPTS", "-Xmx512m"}}
	if !slices.Equal(s.Env, want) {
		t.Errorf("env = %v, want %v", s.Env, want)
	}
	if len(s.Args) != 0 {
		t.Errorf("same command as the image: no args expected, got %v", s.Args)
	}
	flags := strings.Join(s.Flags, " ")
	for _, f := range []string{
		"--restart unless-stopped",
		"--volume /srv/orders/config.yml:/app/config.yml:ro", "--volume orders-data:/data",
		"--tmpfs /tmp:rw,size=64m", "--add-host db.local:10.0.0.5", "--memory 536870912", "--cpus 1.5",
		"--log-driver json-file", "--log-opt max-file=3", "--log-opt max-size=10m", "--label tier=backend",
	} {
		if !strings.Contains(flags, f) {
			t.Errorf("missing %q in %s", f, flags)
		}
	}
	wantPorts := []Port{{HostPort: "8081", Port: "8080/tcp"}, {HostIP: "127.0.0.1", HostPort: "9090", Port: "9090/tcp"}}
	if !slices.Equal(s.Ports, wantPorts) {
		t.Errorf("ports = %+v", s.Ports)
	}
	for _, f := range []string{"--publish", "--hostname", "--memory-swap", "--shm-size", "maintainer", "com.docker.compose", "/cache", "--workdir"} {
		if strings.Contains(flags, f) {
			t.Errorf("unexpected %q in %s", f, flags)
		}
	}
	if s.Bridge || len(s.Networks) != 2 || s.Networks[0].Name != "backend" || !slices.Equal(s.Networks[0].Aliases, []string{"orders"}) {
		t.Errorf("networks = %+v (bridge %v)", s.Networks, s.Bridge)
	}
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "fixed IP") {
		t.Errorf("warnings = %v", s.Warnings)
	}
	if len(s.Binds) != 1 || len(s.Volumes) != 1 || s.Volumes[0] != "orders-data" {
		t.Errorf("binds %v volumes %v", s.Binds, s.Volumes)
	}
}

func TestBuildEntrypoint(t *testing.T) {
	var c inspect
	_ = json.Unmarshal([]byte(sampleContainer), &c)
	c.Config.Entrypoint = []string{"/bin/sh", "-c"}
	c.Config.Cmd = []string{"echo hi"}
	s := build(c, imageConfig{Entrypoint: []string{"/entry.sh"}}, nil)
	if i := slices.Index(s.Flags, "--entrypoint"); i < 0 || s.Flags[i+1] != "/bin/sh" {
		t.Errorf("entrypoint flag missing: %v", s.Flags)
	}
	if !slices.Equal(s.Args, []string{"-c", "echo hi"}) {
		t.Errorf("args = %v", s.Args)
	}
}

func TestValidate(t *testing.T) {
	s := sampleSpec(t)
	bad := []func(*Spec){
		func(s *Spec) { s.Name = "a b" },
		func(s *Spec) { s.Image = "" },
		func(s *Spec) { s.Image = "sha256:abc" },
		func(s *Spec) { s.Env = append(s.Env, EnvVar{"BAD KEY", "x"}) },
		func(s *Spec) { s.Env = append(s.Env, EnvVar{"DB_HOST", "again"}) },
		func(s *Spec) { s.Networks[0].Name = "x;rm -rf /" },
		func(s *Spec) { s.Ports = []Port{{HostPort: "70000", Port: "80/tcp"}} },
		func(s *Spec) { s.Ports = []Port{{HostPort: "80", Port: "80/xyz"}} },
		func(s *Spec) { s.Ports = []Port{{HostIP: "not-an-ip", HostPort: "80", Port: "80/tcp"}} },
	}
	for i, mutate := range bad {
		c := s
		c.Env = slices.Clone(s.Env)
		c.Networks = slices.Clone(s.Networks)
		mutate(&c)
		if _, err := Script(store.Server{}, Plan{Specs: []Spec{c}}, "T"); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
}

// TestScriptRuns runs the generated script with a fake docker that records its arguments, one per line.
func TestScriptRuns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	s := sampleSpec(t)
	s.Env = append(s.Env, EnvVar{"CERT", "line1\nline2"})
	s.Ports[0].HostPort = "18081" // edited in the UI
	other := Spec{Name: "worker", Image: "myorg/worker:1", Env: []EnvVar{}, Bridge: true, Flags: []string{}, Args: []string{"run", "--queue", "a b"}}
	dir := t.TempDir()
	// A mounted .env file that already exists on the target: replaced, the old one kept as .bak.
	appDir := filepath.Join(dir, "app")
	_ = os.MkdirAll(appDir, 0o755)
	envPath := filepath.Join(appDir, ".env")
	_ = os.WriteFile(envPath, []byte("OLD=1\n"), 0o644)
	file := File{Path: envPath, Content: "# prod\nDB_HOST=10.1.0.5\n", Mode: "640", UID: os.Getuid(), GID: os.Getgid()}
	verifyWait = 0
	plan := Plan{Source: "A", Specs: []Spec{s, other}, Files: []File{file}, Options: Options{Images: ImagesAuto}}
	script, err := Script(store.Server{}, plan, "T3st")
	if err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "bin")
	work := filepath.Join(dir, "work")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.MkdirAll(work, 0o700)
	log := filepath.Join(dir, "docker.log")
	// "container inspect" fails: no container exists yet. --env-file is copied so the test can read it after cleanup.
	// After the start, worker has exited (a wrong setting) and says why in its log.
	fake := `#!/bin/sh
if [ "$1" = container ] && [ "$2" = inspect ]; then exit 1; fi
if [ "$1" = inspect ] && [ "$2" = -f ]; then if [ "$4" = worker ]; then echo "exited 1 0"; else echo "running 0 0"; fi; exit 0; fi
if [ "$1" = logs ]; then echo "panic: cannot connect to the database"; exit 0; fi
if [ "$1" = network ] && [ "$2" = inspect ]; then exit 1; fi
prev=""
for a in "$@"; do
  if [ "$prev" = --env-file ]; then cp "$a" "` + dir + `/$(basename "$a").copy"; fi
  prev="$a"
  printf '%s\n' "$a" >> "` + log + `"
done
echo "--" >> "` + log + `"
`
	_ = os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755)
	_ = os.WriteFile(filepath.Join(work, "run.sh"), []byte(script), 0o600)
	_ = os.WriteFile(filepath.Join(work, EnvFileName(0)), []byte(EnvFile(s)), 0o600)
	_ = os.WriteFile(filepath.Join(work, DataFileName(0)), []byte(file.Content), 0o600)

	cmd := exec.Command(sh, filepath.Join(work, "run.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	// The secrets and the script are gone when it ends.
	if left, _ := filepath.Glob(filepath.Join(work, "*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	// Its output, followed as a job, gives the progress.
	job := NewJob("j1", plan, "b", "B", "T3st", nil)
	// The images are the app's part, before the script.
	for i := range PlanImages(plan) {
		job.Image(i, "ok", "pull", "")
	}
	for _, line := range strings.Split(string(out), "\n") {
		job.Line(line)
	}
	job.Finish(nil)
	v := job.View()
	if v.Phase != "done" || v.State != "warning" || v.Percent != 100 {
		t.Errorf("job: phase %s state %s %.0f%%\n%s", v.Phase, v.State, v.Percent, out)
	}
	if f := v.Files[0]; f.Status != "ok" {
		t.Errorf("file: %+v", f)
	}
	if v.Images[0].Status != "ok" || v.Networks[0].Status != "ok" {
		t.Errorf("images %+v networks %+v", v.Images, v.Networks)
	}
	if c := v.Containers[0]; c.Status != "ok" || c.State != "running" {
		t.Errorf("orders-api: %+v", c)
	}
	if c := v.Containers[1]; c.Status != "ok" || c.State != "exited" || c.ExitCode != 1 {
		t.Errorf("worker: %+v", c)
	}
	if l := job.Log("c:1"); !strings.Contains(l, "| panic: cannot connect to the database") {
		t.Errorf("worker log: %q", l)
	}
	raw, _ := os.ReadFile(log)
	calls := strings.Split(strings.TrimSuffix(string(raw), "--\n"), "--\n")
	find := func(prefix ...string) []string {
		for _, c := range calls {
			args := strings.Split(strings.TrimSuffix(c, "\n"), "\n")
			if len(args) >= len(prefix) && slices.Equal(args[:len(prefix)], prefix) {
				return args
			}
		}
		t.Fatalf("no docker %v call in:\n%s", prefix, raw)
		return nil
	}
	find("network", "create", "backend")
	create := find("create", "--env-file")
	// The multi-line value goes as one argument; the others stay in the env file.
	if !strings.Contains(string(raw), "\n--env\nCERT=line1\nline2\n") {
		t.Errorf("multi-line value not passed as one --env argument: %v", create)
	}
	if i := slices.Index(create, "--network"); i < 0 || create[i+1] != "backend" || create[i+2] != "--network-alias" || create[i+3] != "orders" {
		t.Errorf("network flags: %v", create)
	}
	if i := slices.Index(create, "--publish"); i < 0 || create[i+1] != "18081:8080/tcp" || create[i+3] != "127.0.0.1:9090:9090/tcp" {
		t.Errorf("publish flags: %v", create)
	}
	find("network", "connect", "monitoring", "orders-api")
	if got, _ := os.ReadFile(envPath); string(got) != file.Content {
		t.Errorf(".env on the target = %q", got)
	}
	if fi, err := os.Stat(envPath); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf(".env mode: %v %v", fi, err)
	}
	if baks, _ := filepath.Glob(envPath + ".bak-*"); len(baks) != 1 {
		t.Errorf("expected one backup of the old .env, got %v", baks)
	}
	worker := find("create", "--name", "worker")
	if !slices.Equal(worker[len(worker)-4:], []string{"myorg/worker:1", "run", "--queue", "a b"}) {
		t.Errorf("worker args: %v", worker)
	}
	env, err := os.ReadFile(filepath.Join(dir, EnvFileName(0)+".copy"))
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "DB_HOST=10.0.0.5\nDB_PASSWORD=s3cr'et $x\nJAVA_OPTS=-Xmx512m\n" {
		t.Errorf("env file = %q", env)
	}
}

func TestPortArg(t *testing.T) {
	for p, want := range map[Port]string{
		{HostPort: "8080", Port: "80/tcp"}:                        "8080:80/tcp",
		{Port: "80/tcp"}:                                          "80/tcp",
		{HostIP: "127.0.0.1", HostPort: "9090", Port: "9090/udp"}: "127.0.0.1:9090:9090/udp",
		{HostIP: "::1", HostPort: "53", Port: "53/udp"}:           "[::1]:53:53/udp",
		{HostIP: "0.0.0.0", HostPort: "443", Port: "443/tcp"}:     "443:443/tcp",
	} {
		if got := p.arg(); got != want {
			t.Errorf("%+v: %q, want %q", p, got, want)
		}
		if err := p.validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

func TestEnvFileRe(t *testing.T) {
	for p, want := range map[string]bool{"/srv/app/.env": true, "/srv/app/.env.production": true, "/srv/app/api.env": true,
		"/srv/app/config.yml": false, "/srv/.envrc/x": false, "/srv/environment": false} {
		if envFileRe.MatchString(p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}

func TestJobFailureAndCancel(t *testing.T) {
	p := Plan{Source: "A", Specs: []Spec{{Name: "a", Image: "x"}, {Name: "b", Image: "y"}, {Name: "c", Image: "z"}}}
	j := NewJob("j", p, "t", "T", "tg", nil)
	for _, l := range []string{
		"@@tg phase containers", "@@tg c 0 start", "@@tg c 0 step creating", "$ docker create --name a x",
		"Error response from daemon: Conflict. The container name \"/a\" is already in use", "@@tg c 0 failed",
		"@@tg c 1 start", "@@fake c 1 failed", "@@tg c 1 started", "@@tg cancelled",
	} {
		j.Line(l)
	}
	j.Finish(nil)
	v := j.View()
	if v.State != "cancelled" {
		t.Errorf("state %s", v.State)
	}
	if c := v.Containers[0]; c.Status != "failed" || !strings.Contains(c.Error, "already in use") {
		t.Errorf("a: %+v", c)
	}
	// A line with another tag is only log: it cannot fake an event.
	if v.Containers[1].Status != "ok" || v.Containers[2].Status != "cancelled" {
		t.Errorf("b, c: %+v", v.Containers[1:])
	}

	// The output stops early (connection lost): the job says so.
	j2 := NewJob("j2", p, "t", "T", "tg", nil)
	j2.Line("@@tg phase containers")
	j2.Line("@@tg c 0 start")
	j2.Finish(nil)
	if v := j2.View(); v.State != "failed" || v.Containers[0].Status != "unknown" || v.Error == "" {
		t.Errorf("lost: %+v", v)
	}
}

func TestMatchEnvFile(t *testing.T) {
	specs := []Spec{
		{Name: "orders", Env: []EnvVar{{"DB_HOST", "10.0.0.5"}, {"DB_PASS", "p@ss word"}, {"MODE", "\"prod\""}, {"EXTRA", "1"}}},
		{Name: "users", Env: []EnvVar{{"DB_HOST", "10.0.0.5"}, {"DB_PASS", "other"}}},
		{Name: "tz", Env: []EnvVar{{"TZ", "UTC"}}},
	}
	// docker --env-file keeps quotes ("prod" with its quotes); dotenv libraries strip them: both match.
	file := "# orders\nDB_HOST=10.0.0.5\nexport DB_PASS='p@ss word'\nMODE=\"prod\"\n"
	if got := MatchEnvFile(file, specs); !slices.Equal(got, []string{"orders"}) {
		t.Errorf("match = %v", got)
	}
	// One variable proves nothing, unless the container has only that one.
	if got := MatchEnvFile("TZ=UTC\n", specs); !slices.Equal(got, []string{"tz"}) {
		t.Errorf("single = %v", got)
	}
	if got := MatchEnvFile("# nothing\n", specs); len(got) != 0 {
		t.Errorf("empty = %v", got)
	}
	if got := MatchEnvFile("DB_HOST=10.0.0.5\nDB_PASS=wrong\n", specs); len(got) != 0 {
		t.Errorf("wrong value = %v", got)
	}
}

func TestConfigFormat(t *testing.T) {
	for p, want := range map[string]string{
		"/home/deployer/apps/orders/appsettings.json": "json", "/srv/a/.env": "env", "/srv/a/.env.production": "env",
		"/srv/a/app.env": "env", "/home/deployer/.env-gademy": "env", "/srv/a/environment": "", "/etc/app/config.json": "", "/etc/hosts": "", "/usr/share/x.json": "", "/srv/a/config.yml": "",
		"/var/run/docker.sock": "", "/run/secrets/x.json": "",
	} {
		if got := configFormat(p); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}

func TestLoginScript(t *testing.T) {
	for _, ok := range []string{"", "registry.gitlab.com", "gitlab.example.vn:5050", "localhost:5000"} {
		if s, err := LoginScript(store.Server{UseSudo: true}, ok); err != nil || !strings.Contains(s, "sudo -n docker login "+ok) {
			t.Errorf("%q: %v\n%s", ok, err, s)
		}
	}
	for _, bad := range []string{"a b", "x;rm -rf /", "$(id)", "-p secret"} {
		if _, err := LoginScript(store.Server{}, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Services sharing one $ENV leave identical .env-gademy files in every service folder: each container keeps the
// one in its own folder (where its appsettings.json is mounted from, or named after it).
func TestOwnEnvFiles(t *testing.T) {
	specs := []Spec{
		{Name: "gademy-gamesettings-api", Binds: []Bind{{Source: "/home/deployer/gademy-gamesettings-api/appsettings.json"}}},
		{Name: "gademy-chat-api"},
		{Name: "lonely"},
	}
	all := []string{"gademy-gamesettings-api", "gademy-chat-api", "lonely"}
	files := []File{
		{Path: "/home/deployer/gademy-gamesettings-api/.env-gademy", Containers: all},
		{Path: "/home/deployer/gademy-chat-api/.env-gademy", Containers: all},
		{Path: "/home/deployer/gademy-cms-api/.env-gademy", Containers: all},
	}
	got := map[string][]string{}
	for _, f := range ownEnvFiles(files, specs) {
		got[f.Path] = f.Containers
	}
	want := map[string][]string{
		"/home/deployer/gademy-gamesettings-api/.env-gademy": {"gademy-gamesettings-api", "lonely"},
		"/home/deployer/gademy-chat-api/.env-gademy":         {"gademy-chat-api", "lonely"},
		"/home/deployer/gademy-cms-api/.env-gademy":          {"lonely"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for p, w := range want {
		if !slices.Equal(got[p], w) {
			t.Errorf("%s: %v, want %v", p, got[p], w)
		}
	}
}
