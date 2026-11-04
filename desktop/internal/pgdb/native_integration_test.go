package pgdb

// End-to-end test against PostgreSQL installed in WSL (Ubuntu, apt install postgresql), with two clusters:
//
//	sudo pg_createcluster 14 second -p 5433 --start
//	PGDB_WSL=1 go test ./internal/pgdb -run NativeIntegration -v
//
// WSL plays the server: root plays an SSH user allowed to act as postgres, the distribution's default user
// one that is not (sudo asks for a password).

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type wslHost struct {
	user string
	dir  string // a Windows folder for the scripts
}

func (h wslHost) Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	f, err := os.CreateTemp(h.dir, "cmd-*.sh")
	if err != nil {
		return err
	}
	_, _ = f.WriteString(cmd)
	f.Close()
	defer os.Remove(f.Name())
	args := []string{"-d", envOr("PGDB_WSL_DISTRO", "Ubuntu")}
	if h.user != "" {
		args = append(args, "-u", h.user)
	}
	c := exec.CommandContext(ctx, "wsl.exe", append(args, "--", "sh", wslPath(f.Name()))...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return &ExitError{Code: ee.ExitCode()}
	}
	return err
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// wslPath turns C:\Users\x into /mnt/c/Users/x.
func wslPath(p string) string {
	p = filepath.ToSlash(p)
	if len(p) > 2 && p[1] == ':' {
		p = "/mnt/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

func TestNativeIntegration(t *testing.T) {
	if os.Getenv("PGDB_WSL") == "" {
		t.Skip("set PGDB_WSL=1 to run against PostgreSQL installed in WSL")
	}
	dir := t.TempDir()
	root := wslHost{user: "root", dir: dir}
	ctx := context.Background()
	side := &Side{Host: root, Docker: "docker", Server: "wsl"}
	psql := func(id, db, sql string) string {
		t.Helper()
		out, err := run(ctx, root, dexec("docker", id, false, pgEnv+"psql -XAtq -v ON_ERROR_STOP=1 -d "+quote(db)+" <<'SQL'\n"+sql+"\nSQL\n"), 2*time.Minute)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", id, sql, err, out)
		}
		return strings.TrimSpace(out)
	}
	cleanup := func() {
		for _, id := range []string{"native:5432", "native:5433"} {
			_, _ = run(ctx, root, dexec("docker", id, false, pgEnv+`psql -XAtq -d postgres -c 'DROP DATABASE IF EXISTS shop' -c 'DROP DATABASE IF EXISTS shop_copy' -c 'DROP DATABASE IF EXISTS shop_back' -c 'DROP ROLE IF EXISTS shop_owner'`), 2*time.Minute)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	psql("native:5432", "postgres", "CREATE ROLE shop_owner NOLOGIN;\nCREATE DATABASE shop;")
	psql("native:5432", "shop", `
CREATE TABLE customers (id serial PRIMARY KEY, name text NOT NULL);
CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id int REFERENCES customers(id), total numeric(12,2));
CREATE INDEX ON orders (total);
INSERT INTO customers (name) SELECT 'Customer ' || g FROM generate_series(1, 5000) g;
INSERT INTO orders (customer_id, total) SELECT 1 + g % 5000, g % 1000 FROM generate_series(1, 50000) g;
ALTER TABLE orders OWNER TO shop_owner;`)

	t.Run("discovery", func(t *testing.T) {
		list, err := FindInstances(ctx, *side)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Instance{}
		for _, in := range list {
			got[in.Container] = in
		}
		a, b := got["native:5432"], got["native:5433"]
		if !a.Native || a.Port != 5432 || a.Version < 100000 || a.User != "postgres" || !a.Superuser || a.DataDir == "" || b.Port != 5433 {
			t.Fatalf("instances: %+v", list)
		}
		if _, ok := a.Database("shop"); !ok {
			t.Errorf("shop not found: %+v", a.Databases)
		}
		if !strings.Contains(a.PgDump, "pg_dump (PostgreSQL)") {
			t.Errorf("pg_dump of the server: %q", a.PgDump)
		}
		// The default WSL user may not act as postgres: listed with the reason.
		list, err = FindInstances(ctx, Side{Host: wslHost{dir: dir}, Docker: "docker", Server: "wsl"})
		if err != nil || len(list) == 0 || list[len(list)-1].Container != nativeError || !strings.Contains(list[len(list)-1].Error, "may not run commands as postgres") {
			t.Errorf("without sudo: %v %+v", err, list)
		}
	})

	spec := Spec{Kind: "copy", Source: Endpoint{ServerID: "w", Container: "native:5432", Database: "shop"},
		Target: Endpoint{ServerID: "w", Container: "native:5433", Database: "shop_copy"}, KeepOwners: true, Verify: true, Analyze: true}

	t.Run("copy between two clusters, retry what failed", func(t *testing.T) {
		pf := RunPreflight(ctx, spec, side, side)
		if !pf.OK {
			t.Fatalf("preflight: %+v", pf.Checks)
		}
		v := runJob(t, spec, side, side)
		if v.State != "warning" {
			t.Fatalf("state %s: %s\n%v\n%s", v.State, v.Error, v.Warnings, strings.Join(v.Log, "\n"))
		}
		if got := psql("native:5433", "shop_copy", "select count(*) from orders"); got != "50000" {
			t.Errorf("orders: %s", got)
		}
		if v.Summary.Verified != 2 || len(v.Summary.Mismatches) != 0 {
			t.Errorf("verify: %+v", v.Summary)
		}
		for _, p := range v.Phases {
			if p.Key == "transfer" && p.State != "skipped" {
				t.Errorf("one machine: nothing to transfer, got %s", p.State)
			}
		}
		// The owner role is missing on the second cluster.
		var owner []FailedStatement
		for _, f := range v.Failed {
			if strings.Contains(f.Title, "shop_owner") {
				owner = append(owner, f)
			}
		}
		if len(owner) == 0 || !owner[0].Retry || !strings.Contains(owner[0].Command, "OWNER TO shop_owner") {
			t.Fatalf("failed statements: %+v", v.Failed)
		}
		psql("native:5433", "postgres", "CREATE ROLE shop_owner NOLOGIN;")
		fixed := 0
		err := RetryStatements(ctx, side, "native:5433", "shop_copy", "sd-test", owner, func(i int, err error) {
			if err != nil {
				t.Errorf("retry %s: %v", owner[i].Command, err)
			} else {
				fixed++
			}
		})
		if err != nil || fixed != len(owner) {
			t.Fatalf("retry: %v, %d of %d fixed", err, fixed, len(owner))
		}
		if got := psql("native:5433", "shop_copy", "select tableowner from pg_tables where tablename = 'orders'"); got != "shop_owner" {
			t.Errorf("owner after retry: %s", got)
		}
	})

	t.Run("backup to this computer, restore", func(t *testing.T) {
		file := filepath.Join(dir, "shop.pgbackup.tar")
		v := runJob(t, Spec{Kind: "backup", Source: Endpoint{ServerID: "w", Container: "native:5433", Database: "shop_copy"}, File: file}, side, nil)
		if v.State != "done" {
			t.Fatalf("backup %s: %s\n%s", v.State, v.Error, strings.Join(v.Log, "\n"))
		}
		v = runJob(t, Spec{Kind: "restore", Target: Endpoint{ServerID: "w", Container: "native:5432", Database: "shop_back"}, File: file, Analyze: true}, nil, side)
		if v.State != "done" {
			t.Fatalf("restore %s: %s\n%v\n%s", v.State, v.Error, v.Failed, strings.Join(v.Log, "\n"))
		}
		if got := psql("native:5432", "shop_back", "select count(*) from orders"); got != "50000" {
			t.Errorf("orders: %s", got)
		}
		if out, _ := run(ctx, root, "ls /tmp", 2*time.Minute); strings.Contains(out, "sd-") {
			t.Errorf("leftovers in /tmp: %s", out)
		}
	})
}
