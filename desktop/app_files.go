package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"serverdash/internal/ops"
	"serverdash/internal/sshpool"
)

// Files are browsed and edited over SFTP on the existing SSH connection, as the SSH user. Files that
// user cannot read or write (root-owned configs) are read and saved through passwordless sudo when allowed.

const maxEditSize = 2 << 20 // larger files are downloaded rather than edited

type FileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	IsDir   bool   `json:"isDir"`
	IsLink  bool   `json:"isLink"`
	ModTime int64  `json:"modTime"`
}

type DirListing struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

type FileContent struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	Mode    string `json:"mode"`
	Sudo    bool   `json:"sudo"` // read (and will be saved) through sudo
}

func (a *App) sftpOf(serverID string) (*sftp.Client, error) {
	_, conn, err := a.server(serverID)
	if err != nil {
		return nil, err
	}
	return conn.SFTP()
}

// remotePath cleans an absolute server path.
func remotePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r") {
		return "", errors.New("invalid path")
	}
	return path.Clean(p), nil
}

func isPermission(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "permission denied"))
}

// ListDir lists a folder (the user's home folder when dir is empty), folders first.
func (a *App) ListDir(serverID, dir string) (DirListing, error) {
	c, err := a.sftpOf(serverID)
	if err != nil {
		return DirListing{}, err
	}
	if dir == "" {
		if dir, err = c.Getwd(); err != nil || dir == "" {
			dir = "/"
		}
	}
	if dir, err = remotePath(dir); err != nil {
		return DirListing{}, err
	}
	infos, err := c.ReadDir(dir)
	if err != nil {
		if isPermission(err) {
			return DirListing{}, fmt.Errorf("no permission to open %s as this SSH user", dir)
		}
		return DirListing{}, err
	}
	out := DirListing{Path: dir, Entries: make([]FileEntry, 0, len(infos))}
	for _, fi := range infos {
		e := FileEntry{Name: fi.Name(), Size: fi.Size(), Mode: fi.Mode().String(), IsDir: fi.IsDir(), ModTime: fi.ModTime().UnixMilli()}
		if fi.Mode()&os.ModeSymlink != 0 {
			e.IsLink = true
			if st, err := c.Stat(path.Join(dir, fi.Name())); err == nil {
				e.IsDir = st.IsDir()
			}
		}
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		x, y := out.Entries[i], out.Entries[j]
		if x.IsDir != y.IsDir {
			return x.IsDir
		}
		return strings.ToLower(x.Name) < strings.ToLower(y.Name)
	})
	return out, nil
}

// ReadFile opens a text file for editing.
func (a *App) ReadFile(serverID, p string) (FileContent, error) {
	p, err := remotePath(p)
	if err != nil {
		return FileContent{}, err
	}
	_, conn, err := a.server(serverID)
	if err != nil {
		return FileContent{}, err
	}
	c, err := conn.SFTP()
	if err != nil {
		return FileContent{}, err
	}
	st, err := c.Stat(p)
	if err != nil && !isPermission(err) {
		return FileContent{}, err
	}
	out := FileContent{Path: p}
	if st != nil {
		if st.IsDir() {
			return out, errors.New("this is a folder")
		}
		if st.Size() > maxEditSize {
			return out, fmt.Errorf("the file is too large to edit here (%d MB): download it instead", st.Size()>>20)
		}
		out.Size, out.ModTime, out.Mode = st.Size(), st.ModTime().UnixMilli(), st.Mode().String()
	}
	var data []byte
	if f, err := c.Open(p); err == nil {
		data, err = io.ReadAll(io.LimitReader(f, maxEditSize+1))
		_ = f.Close()
		if err != nil {
			return out, err
		}
	} else if isPermission(err) {
		// Root-only file: read it through passwordless sudo when the user may.
		res, xerr := conn.Exec(a.ctx, "sudo -n cat -- "+ops.Quote(p), 30*time.Second)
		if xerr != nil || res.Code != 0 {
			return out, fmt.Errorf("no permission to read %s as this SSH user (and sudo without a password is not allowed)", p)
		}
		data, out.Sudo = []byte(res.Stdout), true
	} else {
		return out, err
	}
	if len(data) > maxEditSize {
		return out, errors.New("the file is too large to edit here: download it instead")
	}
	if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 || !utf8.Valid(data) {
		return out, errors.New("this is a binary file: download it instead")
	}
	out.Content = string(data)
	out.Size = int64(len(data))
	return out, nil
}

// WriteFile saves a file. expectModTime (from ReadFile) protects against overwriting changes made on
// the server meanwhile; 0 skips the check (new file, or the user chose to overwrite).
func (a *App) WriteFile(serverID, p, content string, expectModTime int64) (FileContent, error) {
	p, err := remotePath(p)
	if err != nil {
		return FileContent{}, err
	}
	_, conn, err := a.server(serverID)
	if err != nil {
		return FileContent{}, err
	}
	c, err := conn.SFTP()
	if err != nil {
		return FileContent{}, err
	}
	if st, err := c.Stat(p); err == nil {
		if st.IsDir() {
			return FileContent{}, errors.New("this is a folder")
		}
		if expectModTime != 0 && st.ModTime().UnixMilli() != expectModTime {
			return FileContent{}, errors.New("CHANGED: the file was changed on the server since you opened it")
		}
	}
	sudo := false
	f, err := c.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err == nil {
		_, err = f.Write([]byte(content))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	} else if isPermission(err) {
		// Root-owned file: upload to a temporary file, then copy it over with sudo (keeps the file's owner and mode).
		sudo = true
		err = a.writeWithSudo(conn, c, p, content)
	}
	a.audit(serverID, "file save", p, err)
	if err != nil {
		return FileContent{}, err
	}
	out := FileContent{Path: p, Size: int64(len(content)), Sudo: sudo}
	if st, err := c.Stat(p); err == nil {
		out.ModTime, out.Mode = st.ModTime().UnixMilli(), st.Mode().String()
	}
	return out, nil
}

func (a *App) writeWithSudo(conn *sshpool.Conn, c *sftp.Client, p, content string) error {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	tmp := "/tmp/.serverdash-" + hex.EncodeToString(raw)
	f, err := c.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte(content))
	_ = f.Close()
	if err != nil {
		_ = c.Remove(tmp)
		return err
	}
	res, err := conn.Exec(a.ctx, "sudo -n cp -- "+ops.Quote(tmp)+" "+ops.Quote(p)+"; rc=$?; rm -f "+ops.Quote(tmp)+"; exit $rc", 30*time.Second)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("no permission to write %s as this SSH user (and sudo without a password is not allowed)", p)
	}
	return nil
}

func (a *App) MakeDir(serverID, p string) error {
	p, err := remotePath(p)
	if err != nil {
		return err
	}
	c, err := a.sftpOf(serverID)
	if err != nil {
		return err
	}
	err = c.Mkdir(p)
	a.audit(serverID, "folder create", p, err)
	return err
}

func (a *App) RenamePath(serverID, from, to string) error {
	from, err := remotePath(from)
	if err != nil {
		return err
	}
	if to, err = remotePath(to); err != nil {
		return err
	}
	c, err := a.sftpOf(serverID)
	if err != nil {
		return err
	}
	if _, err := c.Lstat(to); err == nil {
		return fmt.Errorf("%s already exists", path.Base(to))
	}
	err = c.Rename(from, to)
	a.audit(serverID, "rename", from+" → "+to, err)
	return err
}

// DeletePath removes a file, or a folder with everything in it.
func (a *App) DeletePath(serverID, p string) error {
	p, err := remotePath(p)
	if err != nil {
		return err
	}
	if p == "/" {
		return errors.New("refusing to delete /")
	}
	c, err := a.sftpOf(serverID)
	if err != nil {
		return err
	}
	st, err := c.Lstat(p)
	if err == nil {
		if st.IsDir() {
			err = c.RemoveAll(p)
		} else {
			err = c.Remove(p)
		}
	}
	a.audit(serverID, "delete", p, err)
	return err
}

type progress struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	Done     int64  `json:"done"`
	Total    int64  `json:"total"`
}

// copyWithProgress copies and reports progress as "files.progress" events (at most ~5 per second).
func (a *App) copyWithProgress(dst io.Writer, src io.Reader, serverID, name string, total int64) error {
	buf := make([]byte, 256*1024)
	var done int64
	last := time.Time{}
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			done += int64(n)
			if time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				runtime.EventsEmit(a.ctx, "files.progress", progress{serverID, name, done, total})
			}
		}
		if rerr == io.EOF {
			runtime.EventsEmit(a.ctx, "files.progress", progress{serverID, name, total, total})
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// Download saves a server file on this computer (asks where).
func (a *App) Download(serverID, p string) (string, error) {
	p, err := remotePath(p)
	if err != nil {
		return "", err
	}
	c, err := a.sftpOf(serverID)
	if err != nil {
		return "", err
	}
	st, err := c.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", errors.New("folders cannot be downloaded: download the files inside")
	}
	local, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{Title: "Download " + path.Base(p), DefaultFilename: path.Base(p)})
	if err != nil || local == "" {
		return "", err
	}
	src, err := c.Open(p)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.Create(local)
	if err != nil {
		return "", err
	}
	err = a.copyWithProgress(dst, src, serverID, path.Base(p), st.Size())
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(local)
	}
	a.audit(serverID, "download", p, err)
	return local, err
}

// PickUploadFiles asks which local files to upload.
func (a *App) PickUploadFiles() ([]string, error) {
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{Title: "Upload files"})
}

type UploadResult struct {
	Uploaded []string `json:"uploaded"`
	Existing []string `json:"existing"` // not uploaded: already on the server (call again with overwrite)
}

func (a *App) UploadFiles(serverID, dir string, locals []string, overwrite bool) (UploadResult, error) {
	res := UploadResult{Uploaded: []string{}, Existing: []string{}}
	dir, err := remotePath(dir)
	if err != nil {
		return res, err
	}
	c, err := a.sftpOf(serverID)
	if err != nil {
		return res, err
	}
	if !overwrite {
		for _, l := range locals {
			if _, err := c.Lstat(path.Join(dir, filepath.Base(l))); err == nil {
				res.Existing = append(res.Existing, filepath.Base(l))
			}
		}
		if len(res.Existing) > 0 {
			return res, nil
		}
	}
	for _, l := range locals {
		name := filepath.Base(l)
		err := func() error {
			src, err := os.Open(l)
			if err != nil {
				return err
			}
			defer src.Close()
			st, err := src.Stat()
			if err != nil {
				return err
			}
			dst, err := c.OpenFile(path.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
			if err != nil {
				if isPermission(err) {
					return fmt.Errorf("no permission to write in %s as this SSH user", dir)
				}
				return err
			}
			err = a.copyWithProgress(dst, src, serverID, name, st.Size())
			if cerr := dst.Close(); err == nil {
				err = cerr
			}
			return err
		}()
		a.audit(serverID, "upload", path.Join(dir, name), err)
		if err != nil {
			return res, fmt.Errorf("%s: %w", name, err)
		}
		res.Uploaded = append(res.Uploaded, name)
	}
	return res, nil
}
