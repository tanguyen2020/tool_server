package pgdb

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// FileInfo describes a backup file on this computer.
type FileInfo struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Format string `json:"format"` // dir (a .pgbackup.tar made by this app, or a tar of a pg_dump -Fd folder) or custom (pg_dump -Fc)
	Meta   *Meta  `json:"meta"`   // nil for files made elsewhere
	Files  int    `json:"files"`
}

// ReadBackupFile checks that a file can be restored and reads its description.
func ReadBackupFile(path string) (FileInfo, error) {
	info := FileInfo{Path: path}
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return info, err
	}
	info.Bytes = st.Size()
	magic := make([]byte, 5)
	if _, err := io.ReadFull(f, magic); err != nil {
		return info, errors.New("this is not a PostgreSQL backup file")
	}
	if string(magic) == "PGDMP" {
		info.Format = "custom"
		return info, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return info, err
	}
	info.Format = "dir"
	// tar.Reader skips file contents with Seek, so even a large file is read in a moment.
	tr := tar.NewReader(f)
	hasTOC := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if strings.HasSuffix(strings.ToLower(path), ".sql") || info.Files == 0 {
				return info, errors.New("this is not a backup this app can restore: use a .pgbackup.tar made here, a pg_dump -Fc file or a tar of a pg_dump -Fd folder (plain .sql files are not supported)")
			}
			return info, fmt.Errorf("the backup file is damaged: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag != tar.TypeReg {
			continue
		}
		info.Files++
		switch name {
		case "toc.dat":
			hasTOC = true
		case metaName:
			var m Meta
			if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&m); err == nil {
				info.Meta = &m
			}
		}
		if strings.Contains(name, "/") {
			return info, errors.New("the tar file holds a folder: make it from inside the pg_dump folder (tar -cf backup.tar -C dumpdir .)")
		}
	}
	if !hasTOC {
		return info, errors.New("no toc.dat in this tar file: it is not a pg_dump -Fd backup")
	}
	return info, nil
}

// ------------------------------------------------------------------ preflight

// Check is one line of the preflight report.
type Check struct {
	Level string `json:"level"` // ok, warn, error
	Text  string `json:"text"`
}

type Preflight struct {
	Checks       []Check   `json:"checks"`
	OK           bool      `json:"ok"` // no errors
	Source       *Instance `json:"source,omitempty"`
	Target       *Instance `json:"target,omitempty"`
	File         *FileInfo `json:"file,omitempty"`
	TargetExists bool      `json:"targetExists"`
	DumpJobs     int       `json:"dumpJobs"`
	RestoreJobs  int       `json:"restoreJobs"`
	Compression  string    `json:"compression"`
	Direct       *Address  `json:"direct,omitempty"` // where the target reaches the source (direct transfer)
}

// Preflight checks a job before it starts: logins, versions, free space, an existing target database.
func RunPreflight(ctx context.Context, spec Spec, src, dst *Side) Preflight {
	p := Preflight{Checks: []Check{}}
	add := func(level, format string, a ...any) {
		p.Checks = append(p.Checks, Check{level, fmt.Sprintf(format, a...)})
	}
	if err := spec.Validate(); err != nil {
		add("error", "%s", capital(err.Error()))
		return p
	}
	var srcDB Database
	estimate := int64(0) // expected size of the dump
	if spec.hasSource() {
		in, err := Probe(ctx, *src, spec.Source.Container)
		if err != nil {
			add("error", "Source %s / %s: %s", src.Server, spec.Source.Container, err)
		} else {
			p.Source = &in
			db, ok := in.Database(spec.Source.Database)
			if !ok {
				add("error", "Database %s does not exist on %s", spec.Source.Database, src.Server)
			} else {
				srcDB = db
				add("ok", "Source: PostgreSQL %s on %s, database %s (%s)", VersionText(in.Version), src.Server, db.Name, human(db.Bytes))
				p.DumpJobs = autoJobs(spec.Jobs, in)
				_, p.Compression = compressionArg(spec.Compression, in.Major())
				if spec.Compression != "auto" && spec.Compression != p.Compression {
					add("warn", "%s compression needs PostgreSQL 16 or newer on the source: gzip is used", spec.Compression)
				}
				// Compressed table data is usually well under half the database size (indexes are not dumped).
				estimate = db.Bytes / 2
				if spec.SchemaOnly {
					estimate = 64 << 20
				}
				if in.TmpFreeKB > 0 && in.TmpFreeKB*1024 < estimate {
					add("warn", "Only %s free in /tmp of %s on %s; the backup may need up to about %s", human(in.TmpFreeKB*1024), in.Container, src.Server, human(estimate))
				}
				add("ok", "Backup with %d parallel jobs, %s compression", p.DumpJobs, p.Compression)
			}
		}
	}
	if spec.Kind == "restore" {
		fi, err := ReadBackupFile(spec.File)
		if err != nil {
			add("error", "%s", capital(err.Error()))
		} else {
			p.File = &fi
			estimate = fi.Bytes
			if fi.Meta != nil {
				srcDB = Database{Name: fi.Meta.Database, Bytes: fi.Meta.Bytes, Encoding: fi.Meta.Encoding}
				add("ok", "Backup of %s from %s (PostgreSQL %s), %s", fi.Meta.Database, fi.Meta.Server, VersionText(fi.Meta.Version), human(fi.Bytes))
			} else {
				add("ok", "pg_dump backup (%s format), %s", map[string]string{"custom": "custom", "dir": "directory"}[fi.Format], human(fi.Bytes))
			}
		}
	}
	if spec.hasTarget() {
		in, err := Probe(ctx, *dst, spec.Target.Container)
		if err != nil {
			add("error", "Target %s / %s: %s", dst.Server, spec.Target.Container, err)
		} else {
			p.Target = &in
			p.RestoreJobs = autoJobs(spec.restoreWant(), in)
			if spec.Direct && p.Source != nil { // pg_dump runs on the target
				p.DumpJobs = min(p.DumpJobs, autoJobs(spec.Jobs, in))
			}
			add("ok", "Target: PostgreSQL %s on %s, restore with %d parallel jobs", VersionText(in.Version), dst.Server, p.RestoreJobs)
			srcVersion := 0
			if p.Source != nil {
				srcVersion = p.Source.Version
			} else if p.File != nil && p.File.Meta != nil {
				srcVersion = p.File.Meta.Version
			}
			if srcVersion > 0 && in.Major() < srcVersion/10000 {
				add("warn", "The target runs an older PostgreSQL (%s) than the source (%s): restoring to an older version may fail", VersionText(in.Version), VersionText(srcVersion))
			}
			if !in.CreateDB && !in.Superuser {
				add("error", "The PostgreSQL user %s on %s may not create databases", in.User, dst.Server)
			}
			if _, exists := in.Database(spec.Target.Database); exists {
				p.TargetExists = true
				switch spec.IfExists {
				case "stop":
					add("error", "Database %s already exists on %s. Choose another name, or replace it", spec.Target.Database, dst.Server)
				case "replace":
					add("warn", "Database %s already exists on %s: it is dropped once the new copy is complete (it stays usable until then)", spec.Target.Database, dst.Server)
				case "keep":
					add("warn", "Database %s already exists on %s: it is renamed to %s_old_<date> once the new copy is complete", spec.Target.Database, dst.Server, trimName(spec.Target.Database, 40))
				}
			}
			if !sameContainer(spec) && in.TmpFreeKB > 0 && estimate > 0 && in.TmpFreeKB*1024 < estimate {
				add("warn", "Only %s free in /tmp of %s on %s for the backup files (about %s expected)", human(in.TmpFreeKB*1024), in.Container, dst.Server, human(estimate))
			}
			need := srcDB.Bytes // an existing database stays until the new one is complete, so it frees nothing
			if in.DataFree > 0 && need > 0 && in.DataFree*1024 < need {
				add("error", "Not enough disk space for the database on %s: %s free, about %s needed", dst.Server, human(in.DataFree*1024), human(need))
			}
			if spec.KeepOwners {
				add("warn", "Owners and grants are restored: the roles must exist on the target, or those statements fail")
			}
		}
	}
	switch {
	case spec.Direct && p.Source != nil && p.Target != nil:
		p.Direct = checkDirect(ctx, spec, src, dst, p.Source, p.Target, add)
	case spec.Kind == "copy" && !sameContainer(spec):
		add("ok", "The data goes through this computer (the servers do not need to reach each other)")
	}
	p.OK = true
	for _, c := range p.Checks {
		if c.Level == "error" {
			p.OK = false
		}
	}
	return p
}

// checkDirect checks that the target can log in to the source database by itself.
func checkDirect(ctx context.Context, spec Spec, src, dst *Side, srcIn, dstIn *Instance, add func(level, format string, a ...any)) *Address {
	c, _ := detectDirect(ctx, spec, src, dst, srcIn.Major(), dstIn.Major(), 10)
	if !c.OK {
		add("error", "Direct transfer: %s. Untick it to go through this computer", c.Message)
		return nil
	}
	add("ok", "Direct transfer: %s; nothing goes through this computer", c.Message)
	return c.Address
}

func sameContainer(s Spec) bool {
	return s.Kind == "copy" && s.Source.ServerID == s.Target.ServerID && s.Source.Container == s.Target.Container && s.File == ""
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
