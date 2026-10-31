package pgdb

import (
	"context"
	"strings"
	"testing"
)

func TestParseProbe(t *testing.T) {
	out := "mdb=postgres\nuser=app\nnproc=8\nmem=16318644\ntmpfree=51200000\ndatafree=81200000\npgdump=pg_dump (PostgreSQL) 16.4\n@@SQL\n" +
		"version\t160004\nsuper\tfalse\ttrue\nmaxconn\t100\nconns\t7\n" +
		"db\tapp\t123456789\tUTF8\ten_US.utf8\ten_US.utf8\n" +
		"db\tsecret\t-1\tUTF8\tC\tC\n"
	var in Instance
	parseProbe(&in, out)
	if in.Error != "" || in.Version != 160004 || in.Major() != 16 || in.User != "app" || in.Superuser || !in.CreateDB {
		t.Fatalf("instance: %+v", in)
	}
	if in.Cores != 8 || in.TmpFreeKB != 51200000 || in.MaxConn != 100 || in.Conns != 7 || in.MaintDB != "postgres" {
		t.Errorf("resources: %+v", in)
	}
	if len(in.Databases) != 2 || in.Databases[0].Bytes != 123456789 || in.Databases[1].Bytes != -1 || in.Databases[0].Collate != "en_US.utf8" {
		t.Errorf("databases: %+v", in.Databases)
	}

	var bad Instance
	parseProbe(&bad, "mdb=template1\nuser=postgres\n@@SQL\npsql: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: FATAL:  password authentication failed for user \"postgres\"\n")
	if !strings.Contains(bad.Error, "password authentication failed") {
		t.Errorf("login error: %q", bad.Error)
	}
	var none Instance
	parseProbe(&none, "OCI runtime exec failed: exec failed: unable to start container process: exec: \"sh\": executable file not found")
	if none.Error == "" {
		t.Error("no SQL section must be an error")
	}
}

func TestAutoJobs(t *testing.T) {
	cases := []struct {
		want, cores, maxConn, conns, jobs int
	}{
		{0, 8, 0, 0, 4},     // auto: half the cores
		{0, 1, 0, 0, 1},     // at least one
		{0, 3, 0, 0, 1},     // rounds down
		{0, 32, 0, 0, 8},    // at most 8
		{6, 8, 0, 0, 6},     // a chosen number is kept
		{0, 16, 100, 95, 2}, // limited by free connections
	}
	for _, c := range cases {
		if got := autoJobs(c.want, Instance{Cores: c.cores, MaxConn: c.maxConn, Conns: c.conns}); got != c.jobs {
			t.Errorf("want %d, %d cores, %d/%d connections: %d jobs, expected %d", c.want, c.cores, c.conns, c.maxConn, got, c.jobs)
		}
	}
}

func TestCancelQueued(t *testing.T) {
	side := &Side{Server: "a"} // never used: the job must not start
	spec := Spec{Kind: "copy", Source: Endpoint{ServerID: "a", Container: "pg", Database: "shop"}, Target: Endpoint{ServerID: "b", Container: "pg", Database: "shop"}}
	j, err := New("0123456789abcdef", spec, side, side, func() {})
	if err != nil {
		t.Fatal(err)
	}
	j.Queue()
	if j.View().State != "queued" {
		t.Fatalf("state %s", j.View().State)
	}
	j.Cancel()
	select {
	case <-j.Done():
	default:
		t.Fatal("a job cancelled while queued must be done")
	}
	j.Start(context.Background()) // its turn comes: nothing happens
	if v := j.View(); v.State != "cancelled" || v.Phases[0].State != "pending" {
		t.Errorf("a cancelled queued job must not run: %s %s", v.State, v.Phases[0].State)
	}
	j.Cancel() // twice is harmless
}

func TestVersionText(t *testing.T) {
	for v, want := range map[int]string{160004: "16.4", 90624: "9.6.24", 120022: "12.22", 0: "?"} {
		if got := VersionText(v); got != want {
			t.Errorf("%d: got %s want %s", v, got, want)
		}
	}
}

func TestParseTOC(t *testing.T) {
	out := `;
; Archive created at 2026-10-31 10:00:00 UTC
;
215; 1259 16390 TABLE public orders app
216; 1259 16400 SEQUENCE public orders_id_seq app
3350; 0 16390 TABLE DATA public orders app
3351; 0 16410 TABLE DATA sales order lines app
3352; 0 0 SEQUENCE SET public orders_id_seq app
3200; 2606 16420 CONSTRAINT public orders orders_pkey app
3201; 1259 16430 INDEX public orders_created_idx app
3202; 2606 16440 FK CONSTRAINT sales order lines lines_order_fk app
3203; 0 16450 MATERIALIZED VIEW DATA public daily app
3204; 0 16450 MATERIALIZED VIEW public daily app
`
	toc := parseTOC(out)
	kinds := map[int]string{}
	keys := map[int]string{}
	for _, e := range toc {
		kinds[e.ID], keys[e.ID] = e.Kind, e.Key
	}
	want := map[int]string{215: "pre", 216: "pre", 3350: "data", 3351: "data", 3352: "pre", 3200: "post", 3201: "post", 3202: "post", 3203: "post", 3204: "pre"}
	for id, k := range want {
		if kinds[id] != k {
			t.Errorf("item %d: kind %q, want %q", id, kinds[id], k)
		}
	}
	if keys[3350] != "public.orders" || keys[3351] != "sales.order lines" {
		t.Errorf("data keys: %q %q", keys[3350], keys[3351])
	}
}

func TestDataFileSizes(t *testing.T) {
	out := "total 12\n-rw-r--r--    1 0        0          1234 Oct 31 10:00 3350.dat.zst\n" +
		"-rw-r--r--    1 0        0            25 Oct 31 10:00 3351.dat.zst\n" +
		"-rw-r--r--    1 0        0          9999 Oct 31 10:00 toc.dat\n" +
		"-rw-r--r--    1 0        0           512 Oct 31 10:00 sd-backup.json\n" +
		"drwxr-xr-x    2 0        0          4096 Oct 31 10:00 sub\n"
	files := parseLs(out)
	if len(files) != 4 || files["toc.dat"] != 9999 {
		t.Fatalf("files: %v", files)
	}
	sizes := dataFileSizes(files)
	if len(sizes) != 2 || sizes[3350] != 1234 || sizes[3351] != 25 {
		t.Errorf("sizes: %v", sizes)
	}
}

func TestTracker(t *testing.T) {
	tr := newTracker()
	tr.add("public.orders", 1000, 1000)
	tr.add("sales.orders", 500, 500)
	tr.add("public.empty", 0, 0)
	if tr.total != 1501 {
		t.Fatalf("total %d", tr.total)
	}
	tr.start("public.orders")
	tr.poll(map[string]int64{"public.orders": 400})
	done, active := tr.progress()
	if done != 400 || len(active) != 1 || active[0].Name != "public.orders" {
		t.Errorf("running: done %d active %+v", done, active)
	}
	// The COPY is gone from the progress view: finished.
	tr.poll(map[string]int64{})
	if done, _ = tr.progress(); done != 1000 || tr.nDone != 1 {
		t.Errorf("finished by poll: done %d n %d", done, tr.nDone)
	}
	// pg_dump's "finished item" only names the table: the running one wins over the waiting one.
	tr.start("sales.orders")
	tr.finishName("orders")
	if tr.items["sales.orders"].state != 2 {
		t.Error("finishName must pick the running table")
	}
	// COPY progress never shows a table as complete before it finishes.
	tr2 := newTracker()
	tr2.add("a.b", 100, 50)
	tr2.poll(map[string]int64{"a.b": 80})
	if done, _ := tr2.progress(); done != 97 {
		t.Errorf("capped progress: %d", done)
	}
	tr.finishAll()
	if tr.nDone != 3 {
		t.Errorf("finishAll: %d", tr.nDone)
	}
}

func TestVerboseLines(t *testing.T) {
	if m := dumpTableRe.FindStringSubmatch(`pg_dump: dumping contents of table "public.orders"`); m == nil || m[1] != "public.orders" {
		t.Error("dump start")
	}
	if m := finishedRe.FindStringSubmatch(`pg_dump: finished item 3350 TABLE DATA orders`); m == nil || m[1] != "3350" || m[2] != "TABLE DATA orders" {
		t.Error("dump finished")
	}
	if m := launchRe.FindStringSubmatch(`pg_restore: launching item 3201 INDEX orders_created_idx`); m == nil || m[1] != "3201" {
		t.Error("restore launch")
	}
	if m := ignoredRe.FindStringSubmatch(`pg_restore: warning: errors ignored on restore: 3`); m == nil || m[1] != "3" {
		t.Error("errors ignored")
	}
	if !creatingRe.MatchString(`pg_restore: creating FK CONSTRAINT "public.lines lines_order_fk"`) {
		t.Error("creating")
	}
}

func TestParseProgress(t *testing.T) {
	a := parseProgress("public\torders\t1048576\nsales\tlines\t42\nbad line\n")
	if len(a) != 2 || a["public.orders"] != 1048576 || a["sales.lines"] != 42 {
		t.Errorf("progress: %v", a)
	}
}

func TestCompressionArg(t *testing.T) {
	cases := []struct {
		method string
		major  int
		arg    string
	}{
		{"auto", 16, "-Z zstd:3"}, {"lz4", 17, "-Z lz4"}, {"gzip", 16, "-Z gzip:3"}, {"none", 16, "-Z 0"},
		{"auto", 15, "-Z 3"}, {"zstd", 14, "-Z 3"}, {"none", 12, "-Z 0"},
	}
	for _, c := range cases {
		if arg, _ := compressionArg(c.method, c.major); arg != c.arg {
			t.Errorf("%s on %d: %s, want %s", c.method, c.major, arg, c.arg)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Spec{Kind: "copy", Source: Endpoint{ServerID: "a", Container: "pg", Database: "shop"}, Target: Endpoint{ServerID: "b", Container: "pg", Database: "shop"}}
	if err := ok.Validate(); err != nil || ok.IfExists != "stop" || ok.Compression != "auto" {
		t.Fatalf("valid spec: %v %+v", err, ok)
	}
	bad := []Spec{
		{Kind: "nope"},
		{Kind: "copy", Source: Endpoint{ServerID: "a", Container: "pg; rm -rf /", Database: "x"}, Target: ok.Target},
		{Kind: "copy", Source: Endpoint{ServerID: "a", Container: "pg", Database: "host=evil"}, Target: ok.Target},
		{Kind: "copy", Source: ok.Target, Target: ok.Target},
		{Kind: "backup", Source: ok.Source},
		{Kind: "restore", Target: ok.Target, File: "x", IfExists: "maybe"},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("spec %d must be rejected: %+v", i, s)
		}
	}
	b := Spec{Kind: "backup", Source: ok.Source, File: "x", Verify: true}
	if err := b.Validate(); err != nil || b.Verify {
		t.Error("verify only applies to a copy")
	}
}

func TestQuoting(t *testing.T) {
	if got := quote(`it's`); got != `'it'\''s'` {
		t.Errorf("quote: %s", got)
	}
	if ident(`we"ird`) != `"we""ird"` || lit(`o'k`) != `'o''k'` {
		t.Error("sql quoting")
	}
	if got := dexec("sudo -n docker", "pg", true, "echo 'x'"); got != `sudo -n docker exec -i pg sh -c 'echo '\''x'\'''` {
		t.Errorf("dexec: %s", got)
	}
}
