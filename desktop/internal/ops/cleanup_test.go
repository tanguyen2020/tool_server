package ops

import (
	"os/exec"
	"strings"
	"testing"
)

func TestParseSystemCleanup(t *testing.T) {
	out := "@@ROOT:sudo -n\njournal 1200000000 -1\napt-cache 52428800 12\nautoremove -1 3\nrotated-logs 9000 4\ncoredumps 0 0\n"
	info := parseSystemCleanup(out)
	if !info.Root || len(info.Items) != len(cleanupOrder) {
		t.Fatalf("%+v", info)
	}
	by := map[string]CleanupItem{}
	for _, it := range info.Items {
		by[it.Kind] = it
	}
	if j := by["journal"]; j.Bytes != 1.2e9 || j.Count != -1 {
		t.Errorf("journal: %+v", j)
	}
	if a := by["autoremove"]; a.Bytes != -1 || a.Count != 3 {
		t.Errorf("autoremove: %+v", a)
	}
	if x := by["tmp"]; x.Bytes != -1 || x.Count != -1 {
		t.Errorf("missing line must be unknown: %+v", x)
	}
	if parseSystemCleanup("@@ROOT:1000\n").Root {
		t.Error("uid 1000 without sudo is not root")
	}
}

func TestParseContainerLogs(t *testing.T) {
	idA := strings.Repeat("a", 64)
	idB := strings.Repeat("b", 64)
	pathA := "/var/lib/docker/containers/" + idA + "/" + idA + "-json.log"
	out := "@@ROOT:0\n@@INSPECT\n" +
		idA + "\t/web\ttrue\t" + pathA + "\t{\"Type\":\"json-file\",\"Config\":{}}\n" +
		idB + "\t/worker\tfalse\t\t{\"Type\":\"journald\",\"Config\":null}\n" +
		"@@SIZES\n5368709120 " + pathA + "\n"
	r := parseContainerLogs(out)
	if !r.Root || len(r.Logs) != 2 {
		t.Fatalf("%+v", r)
	}
	if a := r.Logs[0]; a.Name != "web" || !a.Running || a.Size != 5368709120 || a.Driver != "json-file" || a.MaxSize != "" {
		t.Errorf("web: %+v", a)
	}
	if b := r.Logs[1]; b.Driver != "journald" || b.Size != -1 || b.Running {
		t.Errorf("worker: %+v", b)
	}
}

func TestLogPathRe(t *testing.T) {
	ok := "/var/lib/docker/containers/" + strings.Repeat("a", 64) + "/" + strings.Repeat("a", 64) + "-json.log"
	if !logPathRe.MatchString(ok) {
		t.Error("docker log path rejected")
	}
	for _, bad := range []string{"", "/etc/passwd", "/x/y.log", "/a b/c-json.log", "/x/$(id)-json.log", "/x/'-json.log"} {
		if logPathRe.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The generated shell scripts must at least parse.
func TestCleanupScriptsSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	scripts := []string{needRoot + "true", AutoremoveScript()}
	for _, k := range cleanupOrder {
		scripts = append(scripts, maybeRoot+measureScripts[k])
	}
	for _, s := range cleanScripts {
		scripts = append(scripts, needRoot+s)
	}
	for _, s := range scripts {
		if out, err := exec.Command(sh, "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%v: %s\n%s", err, out, s)
		}
	}
}
