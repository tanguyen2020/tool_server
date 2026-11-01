package pgdb

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dumpTar makes the tar stream a server sends: the description, toc.dat and data files, padded like GNU tar.
func dumpTar(t *testing.T, meta string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range []struct{ name, body string }{{metaName, meta}, {"toc.dat", "PGDMP toc"}, {"3001.dat.gz", strings.Repeat("row data ", 5000)}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f.body))
	}
	tw.Close()
	b.Write(make([]byte, 8192)) // record padding after the end
	return b.Bytes()
}

func saveBackup(t *testing.T, path, passphrase string, stream []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := newFileSink(f, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(stream); i += 7000 { // in pieces, like a network stream
		sink.Write(stream[i:min(i+7000, len(stream))])
	}
	sum, err := sink.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := writeSidecar(path, sum); err != nil {
		t.Fatal(err)
	}
}

func TestSealPlain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.pgbackup.tar")
	saveBackup(t, path, "", dumpTar(t, `{"format":1,"database":"shop","checksums":true}`))

	info, err := ReadBackupFile(path)
	if err != nil || info.Encrypted || !info.Checksums || !info.Sidecar || info.Files != 3 || info.Meta.Database != "shop" {
		t.Fatalf("info %+v, %v", info, err)
	}
	rep := VerifyBackupFile(context.Background(), path, "")
	if !rep.OK {
		t.Fatalf("verify: %+v", rep.Checks)
	}

	// One flipped byte in the middle of a data file.
	b, _ := os.ReadFile(path)
	i := bytes.Index(b, []byte("row data row"))
	b[i+3] ^= 1
	os.WriteFile(path, b, 0o644)
	rep = VerifyBackupFile(context.Background(), path, "")
	if rep.OK || !hasCheck(rep, "3001.dat.gz does not match") || !hasCheck(rep, "SHA-256 of the file does not match") {
		t.Fatalf("damage not found: %+v", rep.Checks)
	}
	f, _ := os.Open(path)
	c := checkTar(f, false)
	f.Close()
	if c.err() == nil || !strings.Contains(c.err().Error(), "3001.dat.gz") {
		t.Fatalf("checkTar: %+v", c)
	}
}

func TestSealTruncated(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "a.pgbackup.tar")
	saveBackup(t, full, "", dumpTar(t, `{"format":1,"database":"shop","checksums":true}`))
	b, _ := os.ReadFile(full)
	// Cut right before the checksum list: a clean tar boundary, only the description tells it is incomplete.
	cut := filepath.Join(dir, "cut.pgbackup.tar")
	os.WriteFile(cut, b[:bytes.Index(b, []byte(sumsName))], 0o644) // the name starts the member's header
	if _, err := ReadBackupFile(cut); err == nil {
		t.Error("a backup without its checksum list must not be accepted")
	}
	c := checkTar(bytes.NewReader(b[:len(b)/2]), false)
	if c.err() == nil {
		t.Error("half a file must be damaged")
	}
}

func TestSealEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.pgbackup.tar.age")
	saveBackup(t, path, "correct horse battery", dumpTar(t, `{"format":1,"database":"shop","checksums":true}`))
	b, _ := os.ReadFile(path)
	if !bytes.HasPrefix(b, []byte(ageMagic)) || bytes.Contains(b, []byte("row data")) {
		t.Fatal("the file is not encrypted")
	}
	info, err := ReadBackupFile(path)
	if err != nil || !info.Encrypted || info.Meta != nil {
		t.Fatalf("without passphrase: %+v %v", info, err)
	}
	if _, err := ReadBackupFileWith(path, ""); err != errNeedPassphrase {
		t.Errorf("no passphrase: %v", err)
	}
	if _, err := ReadBackupFileWith(path, "wrong passphrase"); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Errorf("wrong passphrase: %v", err)
	}
	info, err = ReadBackupFileWith(path, "correct horse battery")
	if err != nil || info.Format != "dir" || info.Meta == nil || info.Meta.Database != "shop" || !info.Checksums {
		t.Fatalf("with passphrase: %+v %v", info, err)
	}
	if rep := VerifyBackupFile(context.Background(), path, "correct horse battery"); !rep.OK || !rep.Encrypted {
		t.Fatalf("verify: %+v", rep.Checks)
	}
	// Tampering with the ciphertext fails the authentication; dropping the sidecar does not hide it.
	b[len(b)-100] ^= 1
	os.WriteFile(path, b, 0o644)
	os.Remove(sidecarPath(path))
	rep := VerifyBackupFile(context.Background(), path, "correct horse battery")
	if rep.OK || !hasCheck(rep, "damaged or was changed") {
		t.Fatalf("tampering not found: %+v", rep.Checks)
	}
}

func TestStreamCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.pgbackup.tar")
	saveBackup(t, path, "", dumpTar(t, `{"format":1,"checksums":true}`))
	b, _ := os.ReadFile(path)
	sc := newStreamCheck(false)
	sc.Write(b)
	if c := sc.Wait(); c.err() != nil || c.Checked != 3 {
		t.Fatalf("good stream: %+v", c)
	}
	sc.Wait() // twice is fine
	(*streamCheck)(nil).Wait()

	// A tar made elsewhere has no checksums, and that is fine unless its description says otherwise.
	plain := dumpTar(t, `{"format":1}`)
	sc = newStreamCheck(false)
	sc.Write(plain)
	if c := sc.Wait(); c.err() != nil || c.HasSums {
		t.Fatalf("plain stream: %+v", c)
	}
	sc = newStreamCheck(false)
	sc.Write(dumpTar(t, `{"format":1,"checksums":true}`))
	if c := sc.Wait(); c.err() == nil {
		t.Fatal("a stream whose description promises checksums must have them")
	}
}

func hasCheck(r VerifyReport, text string) bool {
	for _, c := range r.Checks {
		if c.Level == "error" && strings.Contains(c.Text, text) {
			return true
		}
	}
	return false
}
