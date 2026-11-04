package pgdb

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// cannedHost answers every command with the same output and error.
type cannedHost struct {
	out  string
	err  error
	cmds []string
}

func (h *cannedHost) Run(_ context.Context, cmd string, _ io.Reader, stdout, _ io.Writer) error {
	h.cmds = append(h.cmds, cmd)
	if stdout != nil {
		_, _ = io.WriteString(stdout, h.out)
	}
	return h.err
}

func TestNativeNames(t *testing.T) {
	for id, port := range map[string]int{"native:5432": 5432, "native:65535": 65535, "native:0": 0, "native:70000": 0, "native": 0, "pg": 0, "native:5432x": 0} {
		if got := nativePort(id); got != port {
			t.Errorf("%s: port %d, want %d", id, got, port)
		}
	}
	if validContainer("native:5432") != nil || validContainer("native:") == nil || validContainer("native:5432;rm") == nil {
		t.Error("validContainer must accept native:<port> only")
	}
	if !strings.Contains(dexec("docker", "native:5433", false, "echo hi"), "sudo -n -u postgres") {
		t.Error("a native server runs as postgres")
	}
	if got := dexec("docker", "pg", false, "echo hi"); !strings.HasPrefix(got, "docker exec pg sh -c") {
		t.Errorf("a container still runs with docker exec: %s", got)
	}
}

func TestSameFiles(t *testing.T) {
	copyOf := func(srv1, c1, srv2, c2 string) Spec {
		return Spec{Kind: "copy", Source: Endpoint{ServerID: srv1, Container: c1, Database: "a"}, Target: Endpoint{ServerID: srv2, Container: c2, Database: "b"}}
	}
	cases := []struct {
		spec Spec
		want bool
	}{
		{copyOf("s1", "pg", "s1", "pg"), true},                   // one container
		{copyOf("s1", "native:5432", "s1", "native:5433"), true}, // two clusters of one machine share /tmp
		{copyOf("s1", "native:5432", "s2", "native:5432"), false},
		{copyOf("s1", "pg", "s1", "native:5432"), false}, // a container has its own /tmp
	}
	for i, c := range cases {
		if got := sameFiles(c.spec); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
	s := copyOf("s1", "native:5432", "s1", "native:5433")
	s.Direct = true
	if err := s.Validate(); err != nil || s.Direct {
		t.Errorf("no direct transfer within one machine: %v %v", err, s.Direct)
	}
}

func TestFindNative(t *testing.T) {
	h := &cannedHost{out: "@@NATIVE\n5432 /usr/lib/postgresql/16/bin /var/lib/postgresql/16/main /var/run/postgresql\n" +
		"5433 /usr/pgsql-14/bin /var/lib/pgsql/14/data /tmp\n"}
	list, err := findNative(context.Background(), Side{Host: h, Docker: "docker"})
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %+v", err, list)
	}
	if list[0].Container != "native:5432" || !list[0].Native || list[0].Port != 5432 || list[0].DataDir != "/var/lib/postgresql/16/main" || list[1].Container != "native:5433" {
		t.Errorf("%+v", list)
	}

	// PostgreSQL runs, but the SSH user may not act as postgres.
	h = &cannedHost{out: "@@NATIVE\nthe SSH user deploy may not run commands as postgres: allow it in /etc/sudoers.d\n", err: &ExitError{Code: 4}}
	list, err = findNative(context.Background(), Side{Host: h, Docker: "docker"})
	if err != nil || len(list) != 1 || list[0].Container != nativeError || !strings.Contains(list[0].Error, "may not run commands as postgres") {
		t.Errorf("%v %+v", err, list)
	}

	// No PostgreSQL outside containers.
	h = &cannedHost{}
	if list, err = findNative(context.Background(), Side{Host: h, Docker: "docker"}); err != nil || len(list) != 0 {
		t.Errorf("%v %+v", err, list)
	}
}

func TestFindInstancesWithoutDocker(t *testing.T) {
	// docker is missing and nothing runs natively: no servers, no error.
	h := &cannedHost{out: "", err: &ExitError{Code: 127}}
	h.out = "sh: 1: docker: not found\n"
	list, err := FindInstances(context.Background(), Side{Host: h, Docker: "docker"})
	if err != nil || len(list) != 0 {
		t.Errorf("%v %+v", err, list)
	}
}

// The generated scripts must at least parse (sh -n), with dash or bash.
func TestNativeScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	scripts := map[string]string{
		"exec":  dexec("docker", "native:5432", true, probeScript),
		"scan":  "[ -n \"$(" + postmasters + ")\" ] || exit 0\necho @@NATIVE\n" + asPostgres(nativeScan+"_pgscan") + " 2>&1",
		"inner": strings.ReplaceAll(nativeEnv, "@PORT@", "5432") + probeScript,
	}
	for name, s := range scripts {
		if out, err := exec.Command(sh, "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", name, err, out)
		}
	}
}
