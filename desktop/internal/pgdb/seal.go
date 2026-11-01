package pgdb

// Backup files on this computer: checksums and encryption.
//
// The tar stream of the dump folder is rewritten as it is saved, member by member, with the SHA-256 of
// every member collected into one more member at the end, sd-checksums.sha256 (sha256sum format). A file
// next to the backup, <file>.sha256, holds the SHA-256 of the whole file as saved, so it can be checked
// with sha256sum without the passphrase. With a passphrase the file is encrypted with age (scrypt), which
// also authenticates every 64 KiB chunk and the end of the file; it can be decrypted without this app:
//
//	age -d backup.pgbackup.tar.age > backup.pgbackup.tar
//
// A restore checks all of it while the file is uploaded, before pg_restore runs.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
)

const (
	sumsName   = "sd-checksums.sha256"
	ageMagic   = "age-encryption.org/v1\n"
	MinPassLen = 8
)

var errNeedPassphrase = errors.New("this backup is encrypted: enter its passphrase")

// isEncrypted tells whether a file starts like an age file.
func isEncrypted(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(ageMagic))
	_, err = io.ReadFull(f, head)
	return err == nil && string(head) == ageMagic
}

// decrypt opens an age stream with a passphrase.
func decrypt(r io.Reader, passphrase string) (io.Reader, error) {
	if passphrase == "" {
		return nil, errNeedPassphrase
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	out, err := age.Decrypt(r, id)
	if err != nil {
		var miss *age.NoIdentityMatchError
		if errors.As(err, &miss) {
			return nil, errors.New("wrong passphrase for this backup")
		}
		return nil, fmt.Errorf("cannot decrypt the backup: %w", err)
	}
	return out, nil
}

// ------------------------------------------------------------------ writing

// fileSink saves the tar stream of a dump folder as a backup file: rewritten with checksums, and
// encrypted when there is a passphrase. Write feeds it; Close finishes the file.
type fileSink struct {
	pw    *io.PipeWriter
	done  chan error
	whole hash.Hash
}

func newFileSink(f io.Writer, passphrase string) (*fileSink, error) {
	s := &fileSink{done: make(chan error, 1), whole: sha256.New()}
	out := io.Writer(io.MultiWriter(f, s.whole))
	var enc io.WriteCloser
	if passphrase != "" {
		r, err := age.NewScryptRecipient(passphrase)
		if err != nil {
			return nil, err
		}
		if enc, err = age.Encrypt(out, r); err != nil {
			return nil, err
		}
		out = enc
	}
	pr, pw := io.Pipe()
	s.pw = pw
	go func() {
		err := rewriteTar(pr, out, enc)
		if err != nil {
			pr.CloseWithError(err)
		}
		_, _ = io.Copy(io.Discard, pr) // the padding after the tar's end
		s.done <- err
	}()
	return s, nil
}

func (s *fileSink) Write(p []byte) (int, error) { return s.pw.Write(p) }

// Close finishes the file and returns the SHA-256 of the whole file as written.
func (s *fileSink) Close() (string, error) {
	s.pw.Close()
	if err := <-s.done; err != nil {
		return "", fmt.Errorf("could not save the backup file: %w", err)
	}
	return hex.EncodeToString(s.whole.Sum(nil)), nil
}

// Abort stops a sink whose stream failed.
func (s *fileSink) Abort() {
	s.pw.CloseWithError(errors.New("cancelled"))
	<-s.done
}

// rewriteTar copies a tar stream member by member and appends the checksums of the members.
func rewriteTar(r io.Reader, w io.Writer, enc io.WriteCloser) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	tr := tar.NewReader(r)
	tw := tar.NewWriter(bw)
	var sums strings.Builder
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(tw, h), tr); err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%x  %s\n", h.Sum(nil), strings.TrimPrefix(hdr.Name, "./"))
	}
	body := sums.String()
	if err := tw.WriteHeader(&tar.Header{Name: sumsName, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.WriteString(tw, body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if enc != nil {
		return enc.Close()
	}
	return nil
}

// sidecarPath is the file holding the SHA-256 of a backup file.
func sidecarPath(path string) string { return path + ".sha256" }

// writeSidecar stores the SHA-256 of a backup file next to it (sha256sum format).
func writeSidecar(path, sum string) error {
	return os.WriteFile(sidecarPath(path), []byte(sum+"  "+filepath.Base(path)+"\n"), 0o644)
}

// readSidecar returns the SHA-256 stored next to a backup file, or "" when there is none.
func readSidecar(path string) string {
	b, err := os.ReadFile(sidecarPath(path))
	if err != nil || len(b) > 4096 {
		return ""
	}
	f := strings.Fields(string(b))
	if len(f) == 0 || len(f[0]) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(f[0]); err != nil {
		return ""
	}
	return strings.ToLower(f[0])
}

// ------------------------------------------------------------------ checking

// tarCheck checks a backup tar stream: every member against sd-checksums.sha256 at its end.
type tarCheck struct {
	Members  int
	Checked  int  // members whose checksum matched
	HasSums  bool // the stream carried sd-checksums.sha256
	HasTOC   bool
	Meta     *Meta
	Problems []string // members that are missing or do not match
	ReadErr  error    // the stream ends too early or is not a tar
}

func (c tarCheck) err() error {
	if c.ReadErr != nil {
		return fmt.Errorf("the backup file is damaged: it ends too early or is not a tar file (%w)", c.ReadErr)
	}
	if len(c.Problems) == 0 {
		return nil
	}
	p := c.Problems
	if len(p) > 3 {
		p = append(p[:3:3], fmt.Sprintf("and %d more", len(c.Problems)-3))
	}
	return errors.New("the backup file is damaged: " + strings.Join(p, "; "))
}

// checkTar reads a whole tar stream. A damaged stream is a problem, not an error; err is for reading.
func checkTar(r io.Reader, wantSums bool) tarCheck {
	var c tarCheck
	got := map[string]string{}
	var sums []byte
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.ReadErr = err
			return c
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		c.Members++
		h := sha256.New()
		var body bytes.Buffer
		var w io.Writer = h
		if name == sumsName || name == metaName {
			w = io.MultiWriter(h, &body)
		}
		if _, err := io.Copy(w, tr); err != nil {
			c.ReadErr = err
			return c
		}
		switch name {
		case sumsName:
			c.HasSums, sums = true, body.Bytes()
			c.Members--
			continue
		case metaName:
			var m Meta
			if json.Unmarshal(body.Bytes(), &m) == nil {
				c.Meta = &m
				wantSums = wantSums || m.Checksums
			}
		case "toc.dat":
			c.HasTOC = true
		}
		got[name] = hex.EncodeToString(h.Sum(nil))
	}
	if !c.HasSums {
		if wantSums {
			c.Problems = append(c.Problems, "its checksum list is missing, so the file is incomplete")
		}
		return c
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(string(sums), "\n") {
		sum, name, ok := strings.Cut(strings.TrimSpace(line), "  ")
		if !ok {
			continue
		}
		listed[name] = true
		switch have, found := got[name]; {
		case !found:
			c.Problems = append(c.Problems, name+" is missing")
		case have != strings.ToLower(sum):
			c.Problems = append(c.Problems, name+" does not match its checksum")
		default:
			c.Checked++
		}
	}
	for name := range got {
		if !listed[name] {
			c.Problems = append(c.Problems, name+" is not in the checksum list")
		}
	}
	return c
}

// streamCheck runs checkTar on a copy of a stream as it goes by. Feed it through Writer; Wait returns
// the result once the stream has ended.
type streamCheck struct {
	pw     *io.PipeWriter
	done   chan tarCheck
	once   sync.Once
	result tarCheck
}

func newStreamCheck(wantSums bool) *streamCheck {
	pr, pw := io.Pipe()
	s := &streamCheck{pw: pw, done: make(chan tarCheck, 1)}
	go func() {
		c := checkTar(pr, wantSums)
		_, _ = io.Copy(io.Discard, pr) // keep the stream flowing whatever happened
		s.done <- c
	}()
	return s
}

func (s *streamCheck) Write(p []byte) (int, error) {
	_, _ = s.pw.Write(p)
	return len(p), nil
}

// Wait ends the stream and returns the result; it may be called again, and on a nil check.
func (s *streamCheck) Wait() tarCheck {
	if s == nil {
		return tarCheck{}
	}
	s.once.Do(func() {
		s.pw.Close()
		s.result = <-s.done
	})
	return s.result
}

// ------------------------------------------------------------------ verify a file

// VerifyReport is the result of checking a backup file from end to end.
type VerifyReport struct {
	OK        bool    `json:"ok"`
	Encrypted bool    `json:"encrypted"`
	Checks    []Check `json:"checks"`
	Meta      *Meta   `json:"meta,omitempty"`
	Bytes     int64   `json:"bytes"`
	Format    string  `json:"format"`
}

// VerifyBackupFile reads a whole backup file and checks its SHA-256, its encryption and every member.
func VerifyBackupFile(ctx context.Context, path, passphrase string) VerifyReport {
	rep := VerifyReport{Checks: []Check{}}
	add := func(level, format string, a ...any) {
		rep.Checks = append(rep.Checks, Check{level, fmt.Sprintf(format, a...)})
	}
	finish := func() VerifyReport {
		rep.OK = true
		for _, c := range rep.Checks {
			if c.Level == "error" {
				rep.OK = false
			}
		}
		return rep
	}
	f, err := os.Open(path)
	if err != nil {
		add("error", "%s", capital(err.Error()))
		return finish()
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		rep.Bytes = st.Size()
	}
	whole := sha256.New()
	raw := io.TeeReader(&ctxReader{ctx, f}, whole)
	rep.Encrypted = isEncrypted(path)
	var plain io.Reader = raw
	if rep.Encrypted {
		if plain, err = decrypt(raw, passphrase); err != nil {
			add("error", "%s", capital(err.Error()))
			return finish()
		}
	}
	br := bufio.NewReaderSize(plain, 64<<10)
	head, _ := br.Peek(5)
	var c tarCheck
	var readErr error
	if string(head) == "PGDMP" {
		rep.Format = "custom"
		_, readErr = io.Copy(io.Discard, br)
	} else {
		rep.Format = "dir"
		c = checkTar(br, false)
		readErr = c.ReadErr
		if readErr == nil {
			_, readErr = io.Copy(io.Discard, br)
		}
	}
	if ctx.Err() != nil {
		add("error", "Cancelled")
		return finish()
	}
	// The rest of the file, so the SHA-256 covers all of it even when the tar ended early.
	_, _ = io.Copy(io.Discard, raw)
	if readErr != nil {
		if rep.Encrypted {
			add("error", "The encrypted file is damaged or was changed: %s", readErr)
		} else {
			add("error", "The file ends too early or is not a backup: %s", readErr)
		}
	} else if rep.Encrypted {
		add("ok", "Decrypted and authenticated: the encrypted data is complete and unchanged")
	}
	if want := readSidecar(path); want != "" {
		if got := hex.EncodeToString(whole.Sum(nil)); got != want {
			add("error", "The SHA-256 of the file does not match %s", filepath.Base(sidecarPath(path)))
		} else {
			add("ok", "The SHA-256 of the file matches %s", filepath.Base(sidecarPath(path)))
		}
	} else {
		add("warn", "No %s next to the file, so the file as a whole was not compared", filepath.Base(sidecarPath(path)))
	}
	if rep.Format == "custom" {
		add("ok", "pg_dump archive (custom format), read to the end")
		return finish()
	}
	rep.Meta = c.Meta
	for _, p := range c.Problems {
		add("error", "%s", capital(p))
	}
	switch {
	case readErr != nil || c.Problems != nil:
	case !c.HasTOC:
		add("error", "No toc.dat: this is not a pg_dump backup")
	case c.HasSums:
		add("ok", "All %d files inside match their checksums", c.Checked)
	default:
		add("warn", "This backup carries no checksums (made by an older version or elsewhere): %d files read without errors", c.Members)
	}
	return finish()
}

// ctxReader stops reading once the context is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
