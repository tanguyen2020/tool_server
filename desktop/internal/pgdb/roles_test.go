package pgdb

import (
	"strings"
	"testing"
)

func TestSetClause(t *testing.T) {
	for in, want := range map[string]string{
		"work_mem=64MB":                 "work_mem = '64MB'",
		"search_path=\"$user\", public": "search_path = \"$user\", public",
		"statement_timeout=0":           "statement_timeout = '0'",
		"app.note=it's":                 "app.note = 'it''s'",
		"search_path=a; DROP TABLE x":   "",
		"bad name=1":                    "",
		"novalue":                       "",
	} {
		if got := setClause(in); got != want {
			t.Errorf("setClause(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoleSQL(t *testing.T) {
	r := Role{Name: `we"ird`, Login: true, Inherit: true, ConnLimit: -1, Password: "SCRAM-SHA-256$4096:abc"}
	got := createRoleSQL(r, 160000)
	want := `CREATE ROLE "we""ird" WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS PASSWORD 'SCRAM-SHA-256$4096:abc'`
	if got != want {
		t.Errorf("create:\n%s\n%s", got, want)
	}
	if got := createRoleSQL(Role{Name: "a", ConnLimit: 5, ValidUntil: "2030-01-01 00:00:00+00"}, 90400); strings.Contains(got, "BYPASSRLS") || !strings.Contains(got, "CONNECTION LIMIT 5 VALID UNTIL '2030-01-01 00:00:00+00'") {
		t.Errorf("old target: %s", got)
	}
	yes, no := true, false
	m := Membership{Role: "readers", Member: "alice", Admin: false, Inherit: &yes, Set: &no}
	if got := grantSQL(m, 160000); got != `GRANT "readers" TO "alice" WITH ADMIN FALSE, INHERIT TRUE, SET FALSE` {
		t.Errorf("grant 16: %s", got)
	}
	m.Admin = true
	if got := grantSQL(m, 150000); got != `GRANT "readers" TO "alice" WITH ADMIN OPTION` {
		t.Errorf("grant 15: %s", got)
	}
}

func TestSteps(t *testing.T) {
	steps := []step{
		{what: "Creating role a", sql: `CREATE ROLE "a"`, role: "a"},
		{what: "Granting", sql: `GRANT "g" TO "a"`},
		{what: "Odd", sql: "select '$sd$'"},
	}
	sql := stepsSQL(steps)
	if strings.Count(sql, "DO $sd$") != 2 || !strings.Contains(sql, "rolname = 'a'") {
		t.Fatalf("sql:\n%s", sql)
	}
	out := "psql:<stdin>:4: NOTICE:  sd|0|created\npsql:<stdin>:8: NOTICE:  sd|1|failed|role \"g\" does not exist\n"
	res := parseSteps(out, len(steps))
	if res[0].state != "created" || res[1].state != "failed" || res[1].msg != `role "g" does not exist` || res[2].state != "" {
		t.Errorf("results: %+v", res)
	}
}

func TestPlanRoles(t *testing.T) {
	g := &Globals{Roles: []Role{{Name: "app"}, {Name: "readers"}, {Name: "alice"}}}
	p := planRoles(g, map[string]bool{"app": true, "postgres": true})
	if len(p.Create) != 2 || p.Create[0].Name != "readers" || len(p.Existing) != 1 || p.Existing[0] != "app" {
		t.Errorf("plan: %+v", p)
	}
}
