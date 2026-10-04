package ops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

// Compose project names: lowercase letters, digits, dashes and underscores (Compose's own rule).
var projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

type ComposeProject struct {
	Name  string   `json:"name"`
	Dir   string   `json:"dir"`
	Files []string `json:"files"`
}

// ComposeInfo finds where a project lives from the labels Compose puts on its containers.
func ComposeInfo(ctx context.Context, conn *sshpool.Conn, srv store.Server, project string) (ComposeProject, error) {
	if !projectRe.MatchString(project) {
		return ComposeProject{}, errors.New("invalid compose project name")
	}
	cmd := fmt.Sprintf(`%s ps -a --filter label=com.docker.compose.project=%s --format '{{.Label "com.docker.compose.project.working_dir"}}|{{.Label "com.docker.compose.project.config_files"}}' | head -n 1`,
		sshpool.DockerBin(srv), project)
	out, err := run(ctx, conn, cmd, "docker ps", 30*time.Second)
	if err != nil {
		return ComposeProject{}, err
	}
	dir, files, _ := strings.Cut(strings.TrimSpace(out), "|")
	if dir == "" {
		return ComposeProject{}, fmt.Errorf("cannot find the folder of project %s (its containers have no compose labels)", project)
	}
	p := ComposeProject{Name: project, Dir: dir, Files: []string{}}
	for _, f := range strings.Split(files, ",") {
		if f = strings.TrimSpace(f); f != "" {
			p.Files = append(p.Files, f)
		}
	}
	if err := validPath(p.Dir); err != nil {
		return ComposeProject{}, fmt.Errorf("unexpected project folder %q", p.Dir)
	}
	for _, f := range p.Files {
		if err := validPath(f); err != nil {
			return ComposeProject{}, fmt.Errorf("unexpected compose file %q", f)
		}
	}
	return p, nil
}

// ComposeActions are the project-level commands, run in a terminal so their output (pull progress) is visible.
var ComposeActions = map[string]struct{ Label, Args string }{
	"update":  {"Pull images and recreate", "pull && dc up -d --remove-orphans"},
	"up":      {"Create and start (up -d)", "up -d"},
	"restart": {"Restart", "restart"},
	"stop":    {"Stop", "stop"},
	"start":   {"Start", "start"},
	"down":    {"Down (remove containers)", "down"},
}

// ComposeScript builds the shell script for a project action. It works with the Compose plugin
// (docker compose) and the standalone docker-compose, through sudo when docker needs it.
func ComposeScript(srv store.Server, p ComposeProject, action string) (string, string, error) {
	a, ok := ComposeActions[action]
	if !ok {
		return "", "", errors.New("invalid compose action")
	}
	docker := sshpool.DockerBin(srv)
	sudo := strings.TrimSuffix(docker, "docker")
	var files strings.Builder
	for _, f := range p.Files {
		files.WriteString(" -f " + Quote(f))
	}
	script := fmt.Sprintf(`cd %s || exit 1
if %s compose version >/dev/null 2>&1; then DC="%s compose"
elif command -v docker-compose >/dev/null 2>&1; then DC="%sdocker-compose"
else echo "Docker Compose is not installed on this server."; exit 1; fi
dc() { $DC -p %s%s "$@"; }
printf '\033[2m$ docker compose -p %s %s\033[0m\n\n'
dc %s`, Quote(p.Dir), docker, docker, sudo, p.Name, files.String(), p.Name, strings.ReplaceAll(a.Args, "dc ", ""), a.Args)
	return banner(script), fmt.Sprintf("%s: %s", p.Name, strings.ToLower(a.Label)), nil
}
