package pgdb

// End-to-end test against real PostgreSQL containers on the local Docker:
//
//	PGDB_IT=1 go test ./internal/pgdb -run Integration -v
//
// Two containers play server A and server B; the local shell plays SSH.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type localHost struct{}

func (localHost) Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = append(os.Environ(), "MSYS_NO_PATHCONV=1") // Git Bash on Windows: keep /tmp/... as is
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return &ExitError{Code: ee.ExitCode()}
	}
	return err
}

const itImage = "postgres:16-alpine"

func sh(t *testing.T, cmd string) string {
	t.Helper()
	var out, errb strings.Builder
	if err := (localHost{}).Run(context.Background(), cmd, nil, &out, &errb); err != nil {
		t.Fatalf("%s: %v\n%s%s", cmd, err, out.String(), errb.String())
	}
	return out.String()
}

func psqlIn(t *testing.T, container, db, sql string) string {
	t.Helper()
	return strings.TrimSpace(sh(t, dexec("docker", container, false, pgEnv+"psql -XAtq -v ON_ERROR_STOP=1 -d "+quote(db)+" <<'SQL'\n"+sql+"\nSQL\n")))
}

func startPG(t *testing.T, name, extra string) {
	t.Helper()
	_ = (localHost{}).Run(context.Background(), "docker rm -f "+name, nil, io.Discard, io.Discard)
	sh(t, "docker run -d --name "+name+" "+extra+" -e POSTGRES_PASSWORD=secret -e POSTGRES_USER=app "+itImage)
	t.Cleanup(func() { _ = (localHost{}).Run(context.Background(), "docker rm -f "+name, nil, io.Discard, io.Discard) })
	for i := 0; i < 60; i++ {
		if (localHost{}).Run(context.Background(), dexec("docker", name, false, pgEnv+"psql -XAtq -d postgres -c 'select 1'"), nil, io.Discard, io.Discard) == nil {
			// The image restarts the server once after init: wait for the final one.
			time.Sleep(2 * time.Second)
			if (localHost{}).Run(context.Background(), dexec("docker", name, false, pgEnv+"psql -XAtq -d postgres -c 'select 1'"), nil, io.Discard, io.Discard) == nil {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s did not start", name)
}

func runJob(t *testing.T, spec Spec, src, dst *Side) View {
	t.Helper()
	j, err := New(newID(), spec, src, dst, func() {})
	if err != nil {
		t.Fatal(err)
	}
	j.Start(context.Background())
	for i := 0; i < 600; i++ {
		v := j.View()
		if v.State != "running" {
			return v
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return View{}
}

var idSeq int

func newID() string {
	idSeq++
	return fmt.Sprintf("%08x%08x", uint32(time.Now().UnixNano()), idSeq)
}

func TestIntegration(t *testing.T) {
	if os.Getenv("PGDB_IT") == "" {
		t.Skip("set PGDB_IT=1 to run against local Docker")
	}
	startPG(t, "sdit-a", "-p 25432:5432") // published, for the direct transfer
	startPG(t, "sdit-b", "-p 25433:5432") // published too: a wrong address must not lead B to itself
	psqlIn(t, "sdit-a", "postgres", "CREATE DATABASE shop;")
	psqlIn(t, "sdit-a", "shop", `
CREATE SCHEMA sales;
CREATE TABLE customers (id serial PRIMARY KEY, name text NOT NULL, email text UNIQUE);
CREATE TABLE sales.orders (id bigserial PRIMARY KEY, customer_id int REFERENCES customers(id), total numeric(12,2), note text, created timestamptz DEFAULT now());
CREATE INDEX ON sales.orders (created);
CREATE TABLE "odd name" (k int);
INSERT INTO customers (name, email) SELECT 'Customer ' || g, 'c' || g || '@example.com' FROM generate_series(1, 20000) g;
INSERT INTO sales.orders (customer_id, total, note) SELECT 1 + g % 20000, g % 1000, repeat(md5(g::text), 8) FROM generate_series(1, 400000) g;
INSERT INTO "odd name" SELECT generate_series(1, 10);
CREATE VIEW sales.big AS SELECT * FROM sales.orders WHERE total > 900;
CREATE MATERIALIZED VIEW sales.daily AS SELECT date_trunc('day', created) d, count(*) FROM sales.orders GROUP BY 1;
`)
	// Containers reach the Docker host (and so the published port of A) as host.docker.internal.
	a := &Side{Host: localHost{}, Docker: "docker", Server: "server-a", Address: "host.docker.internal"}
	b := &Side{Host: localHost{}, Docker: "docker", Server: "server-b"}

	// Preflight finds both instances and the database.
	list, err := FindInstances(context.Background(), *a)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, in := range list {
		if in.Container == "sdit-a" {
			_, found = in.Database("shop")
		}
	}
	if !found {
		t.Fatalf("instances: %+v", list)
	}

	local := filepath.Join(t.TempDir(), "shop.pgbackup.tar")
	copySpec := Spec{Kind: "copy", Source: Endpoint{ServerID: "a", Container: "sdit-a", Database: "shop"},
		Target: Endpoint{ServerID: "b", Container: "sdit-b", Database: "shop"}, File: local, Analyze: true, Verify: true, Jobs: 4}
	pf := RunPreflight(context.Background(), copySpec, a, b)
	if !pf.OK {
		t.Fatalf("preflight: %+v", pf.Checks)
	}

	t.Run("copy", func(t *testing.T) {
		v := runJob(t, copySpec, a, b)
		if v.State != "done" {
			t.Fatalf("state %s: %s\nwarnings: %v\nlog:\n%s", v.State, v.Error, v.Warnings, strings.Join(v.Log, "\n"))
		}
		if v.Summary.Verified != 3 || len(v.Summary.Mismatches) != 0 {
			t.Errorf("verify: %+v", v.Summary)
		}
		if got := psqlIn(t, "sdit-b", "shop", "select count(*) from sales.orders"); got != "400000" {
			t.Errorf("orders on B: %s", got)
		}
		if got := psqlIn(t, "sdit-b", "shop", "select nextval('sales.orders_id_seq')"); got != "400001" {
			t.Errorf("sequence on B: %s", got)
		}
		if got := psqlIn(t, "sdit-b", "shop", "select count(*) from pg_indexes where schemaname = 'sales'"); got != "2" {
			t.Errorf("indexes on B: %s", got)
		}
		for _, p := range v.Phases {
			if p.State != "done" {
				t.Errorf("phase %s: %s", p.Key, p.State)
			}
		}
		if _, err := os.Stat(local); err != nil {
			t.Errorf("local copy: %v", err)
		}
		if out := sh(t, "docker exec sdit-a sh -c 'ls /tmp'") + sh(t, "docker exec sdit-b sh -c 'ls /tmp'"); strings.Contains(out, "sd-") {
			t.Errorf("leftovers: %s", out)
		}
		t.Logf("copy: %.1fs, backup %s, log:\n%s", float64(v.Ended-v.Started)/1000, human(v.Summary.BackupBytes), strings.Join(v.Log[max(0, len(v.Log)-8):], "\n"))
	})

	t.Run("exists", func(t *testing.T) {
		pf := RunPreflight(context.Background(), copySpec, a, b)
		if pf.OK || !pf.TargetExists {
			t.Fatalf("an existing target must stop the job: %+v", pf.Checks)
		}
		s := copySpec
		s.File, s.IfExists = "", "keep"
		psqlIn(t, "sdit-b", "shop", "CREATE TABLE marker (x int);")
		v := runJob(t, s, a, b)
		if v.State != "done" || v.Summary.OldDatabase == "" {
			t.Fatalf("keep: %s %s %+v", v.State, v.Error, v.Summary)
		}
		if got := psqlIn(t, "sdit-b", v.Summary.OldDatabase, "select count(*) from pg_tables where tablename = 'marker'"); got != "1" {
			t.Errorf("the old database must be kept: %s", got)
		}
		s.IfExists = "replace"
		if v := runJob(t, s, a, b); v.State != "done" {
			t.Fatalf("replace: %s %s", v.State, v.Error)
		}
	})

	t.Run("restore file", func(t *testing.T) {
		fi, err := ReadBackupFile(local)
		if err != nil || fi.Meta == nil || fi.Meta.Database != "shop" || fi.Format != "dir" {
			t.Fatalf("file: %+v %v", fi, err)
		}
		s := Spec{Kind: "restore", Target: Endpoint{ServerID: "b", Container: "sdit-b", Database: "shop_restored"}, File: local, Jobs: 2}
		v := runJob(t, s, nil, b)
		if v.State != "done" {
			t.Fatalf("state %s: %s\n%s", v.State, v.Error, strings.Join(v.Log, "\n"))
		}
		if got := psqlIn(t, "sdit-b", "shop_restored", `select count(*) from "odd name"`); got != "10" {
			t.Errorf("odd name: %s", got)
		}
	})

	t.Run("backup", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "b.pgbackup.tar")
		s := Spec{Kind: "backup", Source: Endpoint{ServerID: "a", Container: "sdit-a", Database: "shop"}, File: file, Compression: "lz4"}
		v := runJob(t, s, a, nil)
		if v.State != "done" || v.Summary.Compression != "lz4" {
			t.Fatalf("state %s: %s %+v", v.State, v.Error, v.Summary)
		}
		if _, err := ReadBackupFile(file); err != nil {
			t.Error(err)
		}
	})

	t.Run("custom format", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "shop.dump")
		f, _ := os.Create(file)
		if err := (localHost{}).Run(context.Background(), dexec("docker", "sdit-a", false, pgEnv+"pg_dump -Fc -d shop"), nil, f, io.Discard); err != nil {
			t.Fatal(err)
		}
		f.Close()
		s := Spec{Kind: "restore", Target: Endpoint{ServerID: "b", Container: "sdit-b", Database: "shop_fc"}, File: file}
		if v := runJob(t, s, nil, b); v.State != "done" {
			t.Fatalf("state %s: %s\n%s", v.State, v.Error, strings.Join(v.Log, "\n"))
		}
	})

	t.Run("direct", func(t *testing.T) {
		s := copySpec
		s.File, s.Direct, s.Target.Database = "", true, "shop_direct"
		pf := RunPreflight(context.Background(), s, a, b)
		if !pf.OK || pf.Direct == nil || pf.Direct.Port != 25432 {
			t.Fatalf("preflight: %+v %+v", pf.Checks, pf.Direct)
		}
		v := runJob(t, s, a, b)
		if v.State != "done" {
			t.Fatalf("state %s: %s\n%s", v.State, v.Error, strings.Join(v.Log, "\n"))
		}
		for _, p := range v.Phases {
			if p.Key == "transfer" {
				t.Error("a direct copy has no transfer step")
			}
		}
		if v.Summary.Verified != 3 || len(v.Summary.Mismatches) != 0 {
			t.Errorf("verify: %+v", v.Summary)
		}
		if out := sh(t, "docker exec sdit-b sh -c 'ls -a /tmp'") + sh(t, "docker exec sdit-a sh -c 'ls -a /tmp'"); strings.Contains(out, "sd-") {
			t.Errorf("leftovers (pgpass?): %s", out)
		}
		t.Logf("direct copy: %.1fs", float64(v.Ended-v.Started)/1000)

		if c := DetectDirect(context.Background(), s, a, b); !c.OK || c.Address.Port != 25432 {
			t.Errorf("detect: %+v", c)
		}
		// Same login on both containers: only the identity check tells B's own server from the source.
		self := s
		self.DirectHost, self.DirectPort = "host.docker.internal", 25433
		if c := DetectDirect(context.Background(), self, a, b); c.OK || !strings.Contains(c.Message, "another PostgreSQL server") {
			t.Errorf("the target's own server must be refused: %+v", c)
		}
		bad := s
		bad.DirectPort = 54999
		if pf := RunPreflight(context.Background(), bad, a, b); pf.OK {
			t.Errorf("a wrong port must fail the preflight: %+v", pf.Checks)
		} else {
			t.Logf("wrong port: %v", pf.Checks[len(pf.Checks)-1].Text)
		}
	})

	t.Run("sweep", func(t *testing.T) {
		// Leftovers of a job killed with the app, and the files of a job still running elsewhere.
		dead, alive := "0123456789abcdef", "fedcba9876543210"
		sh(t, "docker exec sdit-b sh -c 'mkdir -p /tmp/sd-"+dead+" && head -c 1048576 /dev/zero > /tmp/sd-"+dead+"/1.dat && echo 99999 > /tmp/sd-"+dead+".pid && mkdir -p /tmp/sd-"+alive+" && touch /tmp/sd-"+alive+".alive'")
		psqlIn(t, "sdit-b", "postgres", "CREATE DATABASE shop_sdnew_012345;")
		psqlIn(t, "sdit-b", "postgres", "CREATE DATABASE shop_sdnew_fedcba;")
		s := Spec{Kind: "restore", Target: Endpoint{ServerID: "b", Container: "sdit-b", Database: "shop_sweep"}, File: local}
		v := runJob(t, s, nil, b)
		if v.State != "done" {
			t.Fatalf("state %s: %s", v.State, v.Error)
		}
		out := sh(t, "docker exec sdit-b sh -c 'ls -a /tmp'")
		if strings.Contains(out, dead) || !strings.Contains(out, alive) {
			t.Errorf("sweep: %s", out)
		}
		dbs := psqlIn(t, "sdit-b", "postgres", "select string_agg(datname, ',' order by datname) from pg_database where datname like '%_sdnew_%'")
		if dbs != "shop_sdnew_fedcba" {
			t.Errorf("temporary databases left: %q", dbs)
		}
		if log := strings.Join(v.Log, "\n"); !strings.Contains(log, "interrupted job") {
			t.Errorf("the sweep must be logged:\n%s", log)
		}
		sh(t, "docker exec sdit-b sh -c 'rm -rf /tmp/sd-"+alive+"*'")
		psqlIn(t, "sdit-b", "postgres", "DROP DATABASE shop_sdnew_fedcba;")
	})

	t.Run("cancel", func(t *testing.T) {
		s := copySpec
		s.File, s.Target.Database = "", "shop_cancel"
		j, _ := New(newID(), s, a, b, func() {})
		j.Start(context.Background())
		for j.View().Phases[1].State != "running" {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		j.Cancel()
		for j.View().State == "running" {
			time.Sleep(100 * time.Millisecond)
		}
		if v := j.View(); v.State != "cancelled" {
			t.Fatalf("state %s", v.State)
		}
		time.Sleep(time.Second)
		if out := sh(t, "docker exec sdit-a sh -c 'ls /tmp; ps'"); strings.Contains(out, "sd-") || strings.Contains(out, "pg_dump") {
			t.Errorf("leftovers after cancel: %s", out)
		}
		if got := psqlIn(t, "sdit-b", "postgres", "select count(*) from pg_database where datname = 'shop_cancel'"); got != "0" {
			t.Errorf("cancelled database left behind")
		}
	})
}
