package pgdb

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileInfo describes a backup file on this computer.
type FileInfo struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Format string `json:"format"` // dir (a .pgbackup.tar made by this app, or a tar of a pg_dump -Fd folder) or custom (pg_dump -Fc)
	Meta   *Meta  `json:"meta"`   // nil for files made elsewhere
	Files  int    `json:"files"`
	// Encrypted files are only described once the passphrase is given (ReadBackupFileWith).
	Encrypted bool `json:"encrypted"`
	Checksums bool `json:"checksums"` // the file carries the checksums of its members (not known for encrypted files)
	Sidecar   bool `json:"sidecar"`   // <file>.sha256 is next to it
}

// ReadBackupFile checks that a file can be restored and reads its description. An encrypted file is
// only recognised as such.
func ReadBackupFile(path string) (FileInfo, error) {
	if isEncrypted(path) {
		info := FileInfo{Path: path, Encrypted: true, Sidecar: readSidecar(path) != ""}
		st, err := os.Stat(path)
		if err != nil {
			return info, err
		}
		info.Bytes = st.Size()
		return info, nil
	}
	return ReadBackupFileWith(path, "")
}

// ReadBackupFileWith is ReadBackupFile that also decrypts an encrypted file with the passphrase. Only the
// start of an encrypted file is read: the description comes first in the backups made here.
func ReadBackupFileWith(path, passphrase string) (FileInfo, error) {
	info := FileInfo{Path: path, Sidecar: readSidecar(path) != ""}
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
	if isEncrypted(path) {
		info.Encrypted = true
		plain, err := decrypt(f, passphrase)
		if err != nil {
			return info, err
		}
		return info, describeStream(plain, &info)
	}
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
		case sumsName:
			info.Checksums = true
			info.Files--
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
	if info.Meta != nil && info.Meta.Checksums && !info.Checksums {
		return info, errors.New("the backup file is incomplete: its checksum list is missing")
	}
	return info, nil
}

// describeStream reads the start of a decrypted backup: its format and description.
func describeStream(r io.Reader, info *FileInfo) error {
	br := bufio.NewReaderSize(r, 64<<10)
	head, err := br.Peek(5)
	if err != nil {
		return errors.New("the encrypted file holds no PostgreSQL backup")
	}
	if string(head) == "PGDMP" {
		info.Format = "custom"
		return nil
	}
	info.Format = "dir"
	tr := tar.NewReader(br)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return errors.New("no toc.dat in the encrypted file: it is not a pg_dump -Fd backup")
		}
		if err != nil {
			return errors.New("the encrypted file holds no backup this app can restore")
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		switch strings.TrimPrefix(h.Name, "./") {
		case metaName:
			var m Meta
			if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&m); err == nil {
				info.Meta = &m
				info.Checksums = m.Checksums
			}
		case "toc.dat":
			return nil // the description, when there is one, comes first
		}
	}
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
	Roles        *RolePlan `json:"roles,omitempty"`  // the roles that would be created on the target
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
	var globals *Globals
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
				if spec.Roles && spec.hasTarget() {
					if g, err := readGlobals(ctx, *src, spec.Source.Container, db.Name, in.Version, in.Superuser); err != nil {
						add("warn", "Could not read the roles of the source: %s", err)
					} else {
						globals = g
					}
				}
			}
		}
	}
	if spec.File != "" && spec.Kind != "restore" {
		if spec.Encrypt {
			add("ok", "The backup file is encrypted with your passphrase (age) and carries checksums: keep the passphrase safe, without it the backup cannot be restored")
		} else {
			add("ok", "The backup file carries checksums, so damage is found before a restore")
		}
	}
	if spec.Kind == "restore" {
		fi, err := ReadBackupFileWith(spec.File, spec.Passphrase)
		if err != nil {
			add("error", "%s", capital(err.Error()))
		} else {
			p.File = &fi
			estimate = fi.Bytes
			if fi.Meta != nil {
				srcDB = Database{Name: fi.Meta.Database, Bytes: fi.Meta.Bytes, Encoding: fi.Meta.Encoding}
				globals = fi.Meta.Globals
				add("ok", "Backup of %s from %s (PostgreSQL %s), %s", fi.Meta.Database, fi.Meta.Server, VersionText(fi.Meta.Version), human(fi.Bytes))
			} else {
				add("ok", "pg_dump backup (%s format), %s", map[string]string{"custom": "custom", "dir": "directory"}[fi.Format], human(fi.Bytes))
			}
			var how []string
			if fi.Encrypted {
				how = append(how, "decrypted and authenticated")
			}
			if fi.Checksums {
				how = append(how, "every file checked against its checksum")
			}
			if fi.Sidecar {
				how = append(how, "the whole file compared with "+filepath.Base(sidecarPath(spec.File)))
			}
			if len(how) > 0 {
				add("ok", "While uploading, the backup is %s", strings.Join(how, ", "))
			} else if fi.Format == "dir" {
				add("warn", "This backup carries no checksums (made by an older version or elsewhere), so damage is found only if pg_restore fails")
			}
			if spec.Roles && (fi.Meta == nil || fi.Meta.Globals == nil) {
				add("warn", "This backup holds no roles (made by an older version or by pg_dump): owners and grants need the roles to exist on the target")
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
			switch {
			case spec.Roles && globals != nil:
				p.Roles = checkRoles(ctx, dst, spec, in, globals, add)
			case spec.KeepOwners && !spec.Roles:
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

// checkRoles says which roles would be created on the target, and whether its login may create them.
func checkRoles(ctx context.Context, dst *Side, spec Spec, in Instance, g *Globals, add func(level, format string, a ...any)) *RolePlan {
	maint := in.MaintDB
	if maint == "" {
		maint = "postgres"
	}
	have, err := existingRoles(ctx, *dst, spec.Target.Container, maint)
	if err != nil {
		add("warn", "Could not read the roles of the target: %s", err)
		return nil
	}
	plan := planRoles(g, have)
	if len(plan.Create) == 0 {
		add("ok", "Roles: all %d roles the database uses exist on %s already", len(g.Roles), dst.Server)
		return &plan
	}
	if !in.Superuser && !in.CreateRole {
		add("error", "The PostgreSQL user %s on %s may not create roles, and %d are missing. Untick creating roles, or log in as a superuser", in.User, dst.Server, len(plan.Create))
		return &plan
	}
	names := make([]string, 0, len(plan.Create))
	var strong []string
	for _, r := range plan.Create {
		names = append(names, r.Name)
		if r.Super || r.Replication || r.BypassRLS {
			strong = append(strong, r.Name+" ("+roleFlags(r)+")")
		}
	}
	text := fmt.Sprintf("Roles: %s created on %s", strings.Join(names, ", "), dst.Server)
	if len(plan.Existing) > 0 {
		text += fmt.Sprintf("; %d exist already and stay as they are", len(plan.Existing))
	}
	add("ok", "%s", text)
	if len(strong) > 0 {
		if in.Superuser {
			add("warn", "These roles are created with powerful attributes, as on the source: %s", strings.Join(strong, ", "))
		} else {
			add("warn", "Only a superuser may create these roles, so they fail: %s", strings.Join(strong, ", "))
		}
	}
	if !g.Passwords {
		add("warn", "Passwords are not copied (the source login is not a superuser, so it cannot read them): set them on the target afterwards")
	}
	return &plan
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
