package pgdb

// Globals: what a database needs from its cluster and pg_dump leaves out.
//
// pg_dump writes the objects of one database with their owners and grants, but not the roles those name,
// and (without --create) not the database's own owner, privileges, settings or comment. They are read from
// the source as JSON and travel in sd-backup.json, so a copy or a restore can put them back on a target:
//
//   - roles: those that own objects in the database or hold privileges in it, the roles that are members
//     of these (the logins that use them), and the roles all of them belong to; with their attributes,
//     password hashes (when the source login may read them) and memberships;
//   - the database's owner, privileges (datacl), settings (ALTER DATABASE / ALTER ROLE … IN DATABASE … SET),
//     connection limit and comment.
//
// Roles that already exist on the target are never changed: only missing ones are created, and a
// membership is granted only when the job created the member or the group.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Globals struct {
	Roles       []Role       `json:"roles"`
	Memberships []Membership `json:"memberships"`
	Passwords   bool         `json:"passwords"` // password hashes were readable on the source
	Owner       string       `json:"owner"`     // owner of the database
	ACLSet      bool         `json:"aclSet"`    // the database has its own privileges (else the defaults)
	ACL         []ACLItem    `json:"acl"`
	Settings    []Setting    `json:"settings"`
	ConnLimit   int          `json:"connLimit"`
	Comment     string       `json:"comment"`
}

type Role struct {
	Name        string `json:"name"`
	Super       bool   `json:"super"`
	Inherit     bool   `json:"inherit"`
	CreateRole  bool   `json:"createRole"`
	CreateDB    bool   `json:"createDb"`
	Login       bool   `json:"login"`
	Replication bool   `json:"replication"`
	BypassRLS   bool   `json:"bypassRls"`
	ConnLimit   int    `json:"connLimit"`
	ValidUntil  string `json:"validUntil,omitempty"`
	Password    string `json:"password,omitempty"` // SCRAM or MD5 hash
}

// Membership is "Member belongs to Role". Inherit and Set are known from PostgreSQL 16 on.
type Membership struct {
	Role    string `json:"role"`
	Member  string `json:"member"`
	Admin   bool   `json:"admin"`
	Inherit *bool  `json:"inherit,omitempty"`
	Set     *bool  `json:"set,omitempty"`
}

// ACLItem is one privilege on the database; an empty grantee is PUBLIC.
type ACLItem struct {
	Grantee   string `json:"grantee"`
	Privilege string `json:"privilege"` // CREATE, CONNECT, TEMPORARY
	Grantable bool   `json:"grantable"`
}

// Setting is a configuration value of a role (everywhere, or in this database only) or of the database
// (Role empty). Value is "name=value" as PostgreSQL stores it.
type Setting struct {
	Role     string `json:"role"`
	Database bool   `json:"database"`
	Value    string `json:"value"`
}

// predefined roles exist in every cluster and are never created.
func predefined(name string) bool { return strings.HasPrefix(name, "pg_") }

// globalsSQL builds the query that returns the globals of the current database as one JSON document.
func globalsSQL(version int, superuser bool) string {
	sys := func(ns string) string {
		return ns + ".nspname not in ('pg_catalog', 'information_schema') and " + ns + ".nspname not like 'pg_toast%' and " + ns + ".nspname not like 'pg_temp%'"
	}
	used := []string{
		"select datdba from pg_database where datname = current_database()",
		"select nspowner from pg_namespace n where " + sys("n"),
		"select relowner from pg_class c join pg_namespace n on n.oid = c.relnamespace where " + sys("n"),
		"select proowner from pg_proc p join pg_namespace n on n.oid = p.pronamespace where " + sys("n"),
		"select typowner from pg_type t join pg_namespace n on n.oid = t.typnamespace where " + sys("n"),
		"select oprowner from pg_operator o join pg_namespace n on n.oid = o.oprnamespace where " + sys("n"),
		"select collowner from pg_collation o join pg_namespace n on n.oid = o.collnamespace where " + sys("n"),
		"select conowner from pg_conversion o join pg_namespace n on n.oid = o.connamespace where " + sys("n"),
		"select dictowner from pg_ts_dict o join pg_namespace n on n.oid = o.dictnamespace where " + sys("n"),
		"select cfgowner from pg_ts_config o join pg_namespace n on n.oid = o.cfgnamespace where " + sys("n"),
		"select a.grantee from pg_namespace n, aclexplode(n.nspacl) a where " + sys("n"),
		"select a.grantee from pg_class c join pg_namespace n on n.oid = c.relnamespace, aclexplode(c.relacl) a where " + sys("n"),
		"select a.grantee from pg_attribute t join pg_class c on c.oid = t.attrelid join pg_namespace n on n.oid = c.relnamespace, aclexplode(t.attacl) a where t.attacl is not null and " + sys("n"),
		"select a.grantee from pg_proc p join pg_namespace n on n.oid = p.pronamespace, aclexplode(p.proacl) a where " + sys("n"),
		"select a.grantee from pg_type t join pg_namespace n on n.oid = t.typnamespace, aclexplode(t.typacl) a where " + sys("n"),
		"select a.grantee from pg_database d, aclexplode(d.datacl) a where d.datname = current_database()",
		"select lomowner from pg_largeobject_metadata",
		"select a.grantee from pg_largeobject_metadata l, aclexplode(l.lomacl) a",
		"select defaclrole from pg_default_acl",
		"select a.grantee from pg_default_acl d, aclexplode(d.defaclacl) a",
		"select evtowner from pg_event_trigger",
		"select fdwowner from pg_foreign_data_wrapper",
		"select srvowner from pg_foreign_server",
		"select a.grantee from pg_foreign_server s, aclexplode(s.srvacl) a",
		"select umuser from pg_user_mappings",
		"select setrole from pg_db_role_setting where setdatabase = (select oid from pg_database where datname = current_database())",
	}
	if version >= 90500 {
		used = append(used, "select unnest(polroles) from pg_policy")
	}
	if version >= 100000 {
		used = append(used, "select pubowner from pg_publication", "select stxowner from pg_statistic_ext")
	}
	bypass := "false"
	if version >= 90500 {
		bypass = "r.rolbypassrls"
	}
	password := "null::text"
	if superuser {
		password = "(select a.rolpassword from pg_authid a where a.oid = r.oid)"
	}
	inherit := ""
	if version >= 160000 {
		inherit = ", 'inherit', m.inherit_option, 'set', m.set_option"
	}
	return `with recursive
used(oid) as (` + strings.Join(used, "\n  union ") + `),
direct as (select u.oid from used u join pg_roles r on r.oid = u.oid where r.rolname !~ '^pg_'),
down(oid) as (select oid from direct union select m.member from pg_auth_members m join down d on m.roleid = d.oid),
up(oid) as (select oid from down union select m.roleid from pg_auth_members m join up u on m.member = u.oid),
s as (select r.oid from pg_roles r where r.oid in (select oid from up) and r.rolname !~ '^pg_'),
db as (select * from pg_database where datname = current_database())
select json_build_object(
  'roles', (select coalesce(json_agg(json_build_object('name', r.rolname, 'super', r.rolsuper, 'inherit', r.rolinherit,
      'createRole', r.rolcreaterole, 'createDb', r.rolcreatedb, 'login', r.rolcanlogin, 'replication', r.rolreplication,
      'bypassRls', ` + bypass + `, 'connLimit', r.rolconnlimit, 'validUntil', r.rolvaliduntil::text, 'password', ` + password + `)
      order by r.rolname), '[]') from pg_roles r where r.oid in (select oid from s)),
  'memberships', (select coalesce(json_agg(json_build_object('role', pg_get_userbyid(m.roleid), 'member', pg_get_userbyid(m.member),
      'admin', m.admin_option` + inherit + `)), '[]')
      from pg_auth_members m where m.member in (select oid from s)
      and m.roleid in (select oid from s union select oid from pg_roles where rolname ~ '^pg_')),
  'passwords', ` + strconv.FormatBool(superuser) + `,
  'owner', (select pg_get_userbyid(datdba) from db),
  'aclSet', (select datacl is not null from db),
  'acl', (select coalesce(json_agg(json_build_object('grantee', case when a.grantee = 0 then '' else pg_get_userbyid(a.grantee) end,
      'privilege', a.privilege_type, 'grantable', a.is_grantable)), '[]') from db, aclexplode(db.datacl) a),
  'settings', (select coalesce(json_agg(json_build_object('role', case when st.setrole = 0 then '' else pg_get_userbyid(st.setrole) end,
      'database', st.setdatabase <> 0, 'value', c)), '[]')
      from pg_db_role_setting st, unnest(st.setconfig) c
      where (st.setrole in (select oid from s) and st.setdatabase in (0, (select oid from db)))
         or (st.setrole = 0 and st.setdatabase = (select oid from db))),
  'connLimit', (select datconnlimit from db),
  'comment', coalesce((select shobj_description(oid, 'pg_database') from db), '')
)::text;`
}

// readGlobals reads the globals of a database on the source.
func readGlobals(ctx context.Context, s Side, container, db string, version int, superuser bool) (*Globals, error) {
	out, err := psqlStdin(ctx, s, container, db, globalsSQL(version, superuser), 2*time.Minute)
	if err != nil {
		return nil, err
	}
	var g Globals
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &g); err != nil {
		return nil, fmt.Errorf("unexpected answer: %s", lastLines(out, 3))
	}
	return &g, nil
}

// psqlStdin runs SQL sent on stdin (never in the shell script, so no text from a database can reach the
// shell) and returns stdout and stderr together, notices included.
func psqlStdin(ctx context.Context, s Side, container, db, sql string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out strings.Builder
	script := pgEnv + "exec psql -XAtq -v ON_ERROR_STOP=1 -d " + quote(db) + " -f - 2>&1"
	err := s.Host.Run(ctx, dexec(s.Docker, container, true, script), strings.NewReader(sql), &out, &out)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out.String(), fmt.Errorf("timed out after %v", timeout)
		}
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return out.String(), fmt.Errorf("%s", friendly(msg))
		}
		return out.String(), err
	}
	return out.String(), nil
}

// existingRoles returns the names of the roles of a server.
func existingRoles(ctx context.Context, s Side, container, maint string) (map[string]bool, error) {
	out, err := psqlStdin(ctx, s, container, maint, "select json_agg(rolname)::text from pg_roles;", time.Minute)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &names); err != nil {
		return nil, fmt.Errorf("unexpected answer: %s", lastLines(out, 3))
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set, nil
}

// RolePlan is what creating the missing roles would do on a target.
type RolePlan struct {
	Create   []Role   `json:"create"`
	Existing []string `json:"existing"`
}

func planRoles(g *Globals, have map[string]bool) RolePlan {
	p := RolePlan{Create: []Role{}, Existing: []string{}}
	for _, r := range g.Roles {
		if have[r.Name] {
			p.Existing = append(p.Existing, r.Name)
		} else {
			p.Create = append(p.Create, r)
		}
	}
	return p
}

// roleFlags describes the powerful attributes of a role ("superuser, login").
func roleFlags(r Role) string {
	var f []string
	if r.Super {
		f = append(f, "superuser")
	}
	if r.Replication {
		f = append(f, "replication")
	}
	if r.BypassRLS {
		f = append(f, "bypasses RLS")
	}
	if r.CreateRole {
		f = append(f, "creates roles")
	}
	if r.Login {
		f = append(f, "login")
	}
	return strings.Join(f, ", ")
}

// ------------------------------------------------------------------ SQL for the target

var (
	gucNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	// Values of list settings are written as stored ("$user", public), so only plain ones are taken.
	gucListRe = regexp.MustCompile(`^[A-Za-z0-9_$". ,/-]*$`)
)

// Settings whose value is a list, written without quotes (as pg_dump does).
var listGUC = map[string]bool{"search_path": true, "temp_tablespaces": true, "session_preload_libraries": true,
	"shared_preload_libraries": true, "local_preload_libraries": true, "unix_socket_directories": true}

// setClause turns "name=value" into "name = value", or "" when it cannot be written safely.
func setClause(v string) string {
	name, value, ok := strings.Cut(v, "=")
	if !ok || !gucNameRe.MatchString(name) {
		return ""
	}
	if listGUC[strings.ToLower(name)] {
		if !gucListRe.MatchString(value) || strings.TrimSpace(value) == "" {
			return ""
		}
		return name + " = " + value
	}
	return name + " = " + lit(value)
}

func createRoleSQL(r Role, version int) string {
	opt := func(on bool, word string) string {
		if on {
			return " " + word
		}
		return " NO" + word
	}
	sql := "CREATE ROLE " + ident(r.Name) + " WITH" + opt(r.Super, "SUPERUSER") + opt(r.Inherit, "INHERIT") + opt(r.CreateRole, "CREATEROLE") +
		opt(r.CreateDB, "CREATEDB") + opt(r.Login, "LOGIN") + opt(r.Replication, "REPLICATION")
	if version >= 90500 {
		sql += opt(r.BypassRLS, "BYPASSRLS")
	}
	if r.ConnLimit != -1 {
		sql += fmt.Sprintf(" CONNECTION LIMIT %d", r.ConnLimit)
	}
	if r.ValidUntil != "" {
		sql += " VALID UNTIL " + lit(r.ValidUntil)
	}
	if r.Password != "" {
		sql += " PASSWORD " + lit(r.Password)
	}
	return sql
}

func grantSQL(m Membership, version int) string {
	sql := "GRANT " + ident(m.Role) + " TO " + ident(m.Member)
	if version >= 160000 {
		var opts []string
		opts = append(opts, "ADMIN "+strings.ToUpper(strconv.FormatBool(m.Admin)))
		if m.Inherit != nil {
			opts = append(opts, "INHERIT "+strings.ToUpper(strconv.FormatBool(*m.Inherit)))
		}
		if m.Set != nil {
			opts = append(opts, "SET "+strings.ToUpper(strconv.FormatBool(*m.Set)))
		}
		return sql + " WITH " + strings.Join(opts, ", ")
	}
	if m.Admin {
		sql += " WITH ADMIN OPTION"
	}
	return sql
}

// step is one statement run on the target; its outcome is reported by a notice, so one failing
// statement neither stops the others nor goes unnoticed.
type step struct {
	what string // for the log and the warnings
	sql  string
	// role creation only: skip when the role exists
	role string
}

// stepsSQL wraps every step in a DO block that reports "sd|<n>|created|exists|ok|failed|<message>".
func stepsSQL(steps []step) string {
	var b strings.Builder
	for i, s := range steps {
		if strings.Contains(s.sql, "$sd$") {
			continue // cannot be quoted; reported as not run
		}
		fmt.Fprintf(&b, "DO $sd$ BEGIN\n")
		if s.role != "" {
			fmt.Fprintf(&b, "IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN RAISE NOTICE 'sd|%d|exists';\nELSE %s; RAISE NOTICE 'sd|%d|created'; END IF;\n", lit(s.role), i, s.sql, i)
		} else {
			fmt.Fprintf(&b, "%s; RAISE NOTICE 'sd|%d|ok';\n", s.sql, i)
		}
		fmt.Fprintf(&b, "EXCEPTION WHEN others THEN RAISE NOTICE 'sd|%d|failed|%%', SQLERRM;\nEND $sd$;\n", i)
	}
	return b.String()
}

var stepRe = regexp.MustCompile(`sd\|(\d+)\|(created|exists|ok|failed)(?:\|(.*))?`)

// stepResult is the outcome of one step: created, exists, ok, failed or "" (no answer).
type stepResult struct {
	state, msg string
}

func parseSteps(out string, n int) []stepResult {
	res := make([]stepResult, n)
	for _, line := range strings.Split(out, "\n") {
		m := stepRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		if i >= 0 && i < n {
			res[i] = stepResult{m[2], strings.TrimSpace(m[3])}
		}
	}
	return res
}

// ------------------------------------------------------------------ in a job

// runSteps runs steps on the target's database db and returns their outcomes; failures become warnings.
func (j *Job) runSteps(ctx context.Context, db string, steps []step) []stepResult {
	if len(steps) == 0 {
		return nil
	}
	out, err := psqlStdin(ctx, *j.dst, j.spec.Target.Container, db, stepsSQL(steps), 10*time.Minute)
	res := parseSteps(out, len(steps))
	for i, r := range res {
		switch r.state {
		case "failed":
			j.warn(fmt.Sprintf("%s failed: %s", steps[i].what, r.msg))
		case "":
			msg := "not run"
			if err != nil {
				msg = err.Error()
			}
			j.warn(fmt.Sprintf("%s: %s", steps[i].what, msg))
			res[i].state = "failed"
		}
	}
	return res
}

// createRoles creates the roles of the source that the target lacks, then their memberships and settings.
func (j *Job) createRoles(ctx context.Context) error {
	g := j.meta.Globals
	if g == nil {
		j.warn("This backup holds no roles (it was made by an older version or by pg_dump): owners and grants need the roles to exist on the target.")
		return nil
	}
	maint := j.dstInfo.MaintDB
	if maint == "" {
		maint = "postgres"
	}
	have, err := existingRoles(ctx, *j.dst, j.spec.Target.Container, maint)
	if err != nil {
		return fmt.Errorf("could not read the roles of the target: %w", err)
	}
	plan := planRoles(g, have)
	j.mu.Lock()
	j.v.Summary.RolesExisting = len(plan.Existing)
	j.mu.Unlock()
	if len(plan.Create) == 0 {
		j.log(fmt.Sprintf("Roles: all %d roles exist on the target already", len(g.Roles)))
		return nil
	}
	steps := make([]step, len(plan.Create))
	for i, r := range plan.Create {
		steps[i] = step{what: "Creating role " + r.Name, sql: createRoleSQL(r, j.dstInfo.Version), role: r.Name}
	}
	res := j.runSteps(ctx, maint, steps)
	created := map[string]bool{}
	var names []string
	for i, r := range res {
		if r.state == "created" {
			created[plan.Create[i].Name] = true
			names = append(names, plan.Create[i].Name)
		}
	}
	j.mu.Lock()
	j.v.Summary.RolesCreated = names
	j.mu.Unlock()
	if len(names) > 0 {
		j.log("Created roles: " + strings.Join(names, ", "))
		if !g.Passwords {
			j.warn("Passwords were not copied (the source login is not a superuser, so it cannot read them): set them on the target for " + strings.Join(names, ", "))
		}
	}
	// Memberships and role-wide settings, only for what this job created: existing roles stay as they are.
	steps = steps[:0]
	for _, m := range g.Memberships {
		if !(created[m.Member] || created[m.Role]) || !(have[m.Member] || created[m.Member]) || !(have[m.Role] || created[m.Role] || predefined(m.Role)) {
			continue
		}
		steps = append(steps, step{what: "Granting " + m.Role + " to " + m.Member, sql: grantSQL(m, j.dstInfo.Version)})
	}
	for _, s := range g.Settings {
		if s.Role == "" || s.Database || !created[s.Role] {
			continue
		}
		if c := setClause(s.Value); c != "" {
			steps = append(steps, step{what: "Setting " + c + " for " + s.Role, sql: "ALTER ROLE " + ident(s.Role) + " SET " + c})
		}
	}
	j.runSteps(ctx, maint, steps)
	return nil
}

// databaseProps gives the restored database the source's settings, connection limit and comment, and with
// the owners kept, its owner, privileges and the role settings in it. db is the database before renaming.
func (j *Job) databaseProps(ctx context.Context, db string) {
	g := j.meta.Globals
	if g == nil {
		return
	}
	d := "DATABASE " + ident(db)
	var steps []step
	if j.spec.KeepOwners {
		if g.Owner != "" {
			steps = append(steps, step{what: "Making " + g.Owner + " the owner of the database", sql: "ALTER " + d + " OWNER TO " + ident(g.Owner)})
		}
		if g.ACLSet {
			steps = append(steps, step{what: "Revoking the default database privileges", sql: "REVOKE ALL ON " + d + " FROM PUBLIC"})
			for _, a := range g.ACL {
				switch a.Privilege {
				case "CREATE", "CONNECT", "TEMPORARY":
				default:
					continue
				}
				to := "PUBLIC"
				if a.Grantee != "" {
					to = ident(a.Grantee)
				}
				sql := "GRANT " + a.Privilege + " ON " + d + " TO " + to
				if a.Grantable {
					sql += " WITH GRANT OPTION"
				}
				steps = append(steps, step{what: "Granting " + a.Privilege + " on the database to " + strings.Trim(to, `"`), sql: sql})
			}
		}
	}
	for _, s := range g.Settings {
		c := setClause(s.Value)
		switch {
		case c == "" || !s.Database:
		case s.Role == "":
			steps = append(steps, step{what: "Setting " + c + " on the database", sql: "ALTER " + d + " SET " + c})
		case j.spec.KeepOwners:
			steps = append(steps, step{what: "Setting " + c + " for " + s.Role + " in the database", sql: "ALTER ROLE " + ident(s.Role) + " IN " + d + " SET " + c})
		}
	}
	if g.ConnLimit > 0 {
		steps = append(steps, step{what: "Setting the connection limit of the database", sql: fmt.Sprintf("ALTER %s CONNECTION LIMIT %d", d, g.ConnLimit)})
	}
	if g.Comment != "" {
		steps = append(steps, step{what: "Setting the comment of the database", sql: "COMMENT ON " + d + " IS " + lit(g.Comment)})
	}
	maint := j.dstInfo.MaintDB
	if maint == "" {
		maint = "postgres"
	}
	if len(steps) > 0 {
		j.detail("finish", "Owner, privileges and settings of the database")
		j.runSteps(ctx, maint, steps)
	}
}
