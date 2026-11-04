package pgdb

import (
	"strings"
	"testing"
)

func TestFailureParser(t *testing.T) {
	out := []string{
		"pg_restore: creating INDEX \"public.ix_dataset_chunks_embedding\"",
		"pg_restore: from TOC entry 4219; 1259 16405 INDEX ix_dataset_chunks_embedding app",
		`pg_restore: error: could not execute query: ERROR:  could not resize shared memory segment "/PostgreSQL.2877226648" to 1070633248 bytes: No space left on device`,
		"Command was: CREATE INDEX ix_dataset_chunks_embedding ON public.dataset_chunks USING hnsw (embedding public.vector_cosine_ops);",
		"pg_restore: creating FUNCTION \"public.touch()\"",
		"pg_restore: from TOC entry 300; 1255 16500 FUNCTION touch() app",
		`pg_restore: error: could not execute query: ERROR:  role "reporting" does not exist`,
		"Command was: CREATE FUNCTION public.touch() RETURNS trigger",
		"    LANGUAGE plpgsql",
		"    AS $$ begin new.updated := now(); return new; end $$;",
		"pg_restore: from TOC entry 5; 2615 2200 SCHEMA public pg_database_owner",
		`pg_restore: error: could not execute query: ERROR:  schema "public" already exists`,
		"Command was: CREATE SCHEMA public;",
		"pg_restore: warning: errors ignored on restore: 3",
	}
	var p failureParser
	for _, l := range out {
		p.line(l)
	}
	got := p.result()
	if len(got) != 3 {
		t.Fatalf("want 3 failures, got %d: %+v", len(got), got)
	}
	shm := got[0]
	if shm.Object != "INDEX ix_dataset_chunks_embedding" || !strings.HasPrefix(shm.Error, "could not resize shared memory segment") ||
		!strings.HasPrefix(shm.Command, "CREATE INDEX ix_dataset_chunks_embedding") || !shm.Retry {
		t.Errorf("shm: %+v", shm)
	}
	if !strings.Contains(shm.Title, "shared memory") || !strings.Contains(shm.Fix, "shm_size") || shm.Again == "" {
		t.Errorf("shm explanation: %+v", shm)
	}
	fn := got[1]
	if fn.Command != "CREATE FUNCTION public.touch() RETURNS trigger\n    LANGUAGE plpgsql\n    AS $$ begin new.updated := now(); return new; end $$;" {
		t.Errorf("multi-line command: %q", fn.Command)
	}
	if fn.Title != "The role reporting does not exist on the destination" || !fn.Retry {
		t.Errorf("role: %+v", fn)
	}
	if exists := got[2]; exists.Retry || exists.Object != "SCHEMA public" {
		t.Errorf("an existing object is not run again: %+v", exists)
	}
}

func TestFailureParserDetailAndOldFormat(t *testing.T) {
	var p failureParser
	for _, l := range []string{
		`pg_restore: [archiver (db)] could not execute query: ERROR:  syntax error at or near "STORED"`,
		"LINE 3:     total numeric GENERATED ALWAYS AS (a + b) STORED",
		"                                                      ^",
		"    Command was: CREATE TABLE public.t (",
		"    total numeric GENERATED ALWAYS AS (a + b) STORED",
		");",
	} {
		p.line(l)
	}
	got := p.result()
	if len(got) != 1 {
		t.Fatalf("want 1, got %+v", got)
	}
	f := got[0]
	if f.Error != `syntax error at or near "STORED"` || !strings.HasPrefix(f.Detail, "LINE 3:") || !strings.HasSuffix(f.Command, ");") {
		t.Errorf("%+v", f)
	}
	if f.Title != `Syntax error at or near "STORED"` || f.Fix != "" {
		t.Errorf("an unknown error keeps its message: %+v", f)
	}
}

func TestExplainJobErrorMentionsNoRetry(t *testing.T) {
	for _, msg := range []string{
		"could not resize shared memory segment \"/PostgreSQL.1\" to 1 bytes: No space left on device",
		"ERROR: out of memory",
		"canceling statement due to statement timeout",
	} {
		if _, fix := Explain(msg); fix == "" || strings.Contains(fix, "Retry") {
			t.Errorf("%s: %q", msg, fix)
		}
	}
}

func TestIndexSettings(t *testing.T) {
	gb := int64(1 << 20) // in KB
	cases := []struct {
		name       string
		in         Instance
		jobs       int
		mwm        int64
		noParallel bool
	}{
		{"docker default 64 MB", Instance{MemKB: 32 * gb, ShmKB: 65536, DSMType: "posix"}, 4, 1024, true},
		{"shm_size 8g", Instance{MemKB: 32 * gb, ShmKB: 8 * gb, DSMType: "posix"}, 4, 1024, false},
		{"small server", Instance{MemKB: 2 * gb, ShmKB: 65536, DSMType: "posix"}, 2, 256, true},
		{"unknown shm", Instance{MemKB: 32 * gb}, 4, 1024, false},
		{"not in /dev/shm", Instance{MemKB: 32 * gb, ShmKB: 65536, DSMType: "sysv"}, 4, 1024, false},
		{"unknown memory", Instance{ShmKB: gb, DSMType: "posix"}, 1, 256, false},
	}
	for _, c := range cases {
		mwm, np := indexSettings(c.in, c.jobs)
		if mwm != c.mwm || np != c.noParallel {
			t.Errorf("%s: %d MB, no parallel %v; want %d MB, %v", c.name, mwm, np, c.mwm, c.noParallel)
		}
	}
}

func TestPsqlError(t *testing.T) {
	got := psqlError("psql:<stdin>:6: ERROR:  could not resize shared memory segment \"/PostgreSQL.1\" to 9 bytes: No space left on device\n", nil)
	if got != `could not resize shared memory segment "/PostgreSQL.1" to 9 bytes: No space left on device` {
		t.Errorf("%q", got)
	}
}
