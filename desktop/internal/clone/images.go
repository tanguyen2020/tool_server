package clone

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"serverdash/internal/ops"
	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

// Side is one server of a copy: its settings and its connection.
type Side struct {
	Srv  store.Server
	Conn *sshpool.Conn
}

// GetImages makes sure the target has every image before the containers are created, by mode: pull it, copy it
// from the source (docker save | docker load through this computer: no registry or login needed), or check it is
// there. Progress goes to j; it stops when ctx is cancelled. src may be nil when the mode never copies.
func GetImages(ctx context.Context, j *Job, images []ImageRef, mode string, src *Side, dst Side) {
	for i, img := range images {
		if ctx.Err() != nil {
			return
		}
		j.Image(i, "working", "", "")
		have := imageID(ctx, dst, img.Name)
		exact := img.ID != "" && have == img.ID
		fail := func(err error) {
			if ctx.Err() == nil {
				j.ImageLog(i, err.Error())
				j.Image(i, "failed", "", err.Error())
			}
		}
		copyIt := func() {
			if src == nil {
				fail(errors.New("the source server is not connected"))
				return
			}
			if err := copyImage(ctx, j, i, img, *src, dst); err != nil {
				fail(err)
				return
			}
			j.Image(i, "ok", "copy", "")
		}
		switch mode {
		case ImagesLocal:
			if have == "" {
				fail(errors.New("not on the target server (choose another way to get the images)"))
			} else {
				j.ImageLog(i, "Already on the target: "+have)
				j.Image(i, "ok", "local", "")
			}
		case ImagesPull:
			if err := pullImage(ctx, j, i, img.Name, dst); err != nil {
				fail(err)
			} else {
				j.Image(i, "ok", "pull", "")
			}
		case ImagesCopy:
			if exact {
				j.ImageLog(i, "The target already has this exact image ("+short(have)+"): nothing to copy.")
				j.Image(i, "ok", "local", "")
			} else {
				copyIt()
			}
		default: // auto
			switch {
			case exact:
				j.ImageLog(i, "The target already has this exact image ("+short(have)+").")
				j.Image(i, "ok", "local", "")
			case pullImage(ctx, j, i, img.Name, dst) == nil:
				j.Image(i, "ok", "pull", "")
			case ctx.Err() == nil:
				j.ImageLog(i, "Pull failed: copying it from the source server instead.")
				copyIt()
			}
		}
	}
}

// imageID is the ID of an image on a server ("" when it does not have it).
func imageID(ctx context.Context, s Side, ref string) string {
	res, err := s.Conn.Exec(ctx, sshpool.DockerBin(s.Srv)+" image inspect --format '{{.Id}}' -- "+ops.Quote(ref)+" 2>/dev/null", 30*time.Second)
	if err != nil || res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

func pullImage(ctx context.Context, j *Job, i int, name string, dst Side) error {
	j.ImageLog(i, "$ docker pull "+name)
	st, err := dst.Conn.Stream(sshpool.DockerBin(dst.Srv) + " pull -- " + ops.Quote(name) + " 2>&1")
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, st.Stop)
	defer stop()
	var last string
	sc := bufio.NewScanner(st)
	for sc.Scan() {
		last = strings.TrimSpace(sc.Text())
		j.ImageLog(i, sc.Text())
	}
	if err := st.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if last != "" {
			return errors.New(last)
		}
		return fmt.Errorf("docker pull failed: %w", err)
	}
	return nil
}

// copyImage streams the image from the source to the target. The source runs it by ID when it can, so the target
// gets the build the source runs even if the tag has moved since; the target then gives it its name.
func copyImage(ctx context.Context, j *Job, i int, img ImageRef, src, dst Side) error {
	ref := img.Name
	if img.ID != "" {
		ref = img.ID
	}
	size := int64(0)
	if res, err := src.Conn.Exec(ctx, sshpool.DockerBin(src.Srv)+" image inspect --format '{{.Size}}' -- "+ops.Quote(ref), 30*time.Second); err == nil && res.Code == 0 {
		size, _ = strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	} else if img.ID != "" {
		ref, img.ID = img.Name, "" // the source no longer has that build: copy what the name points to there
	}
	j.ImageLog(i, fmt.Sprintf("$ docker save %s | gzip | docker load   (from %s to %s, through this computer)", ref, src.Srv.Name, dst.Srv.Name))

	// docker save fails into stderr while gzip still ends well: the marker tells.
	save := fmt.Sprintf(`if command -v gzip >/dev/null 2>&1; then { %[1]s save -- %[2]s || echo "@@save-failed" >&2; } | gzip -1; else %[1]s save -- %[2]s || echo "@@save-failed" >&2; fi`,
		sshpool.DockerBin(src.Srv), ops.Quote(ref))
	from, err := src.Conn.Start(save)
	if err != nil {
		return fmt.Errorf("cannot start docker save on %s: %w", src.Srv.Name, err)
	}
	defer from.Close()
	to, err := dst.Conn.Start(sshpool.DockerBin(dst.Srv) + " load")
	if err != nil {
		return fmt.Errorf("cannot start docker load on %s: %w", dst.Srv.Name, err)
	}
	defer to.Close()
	stop := context.AfterFunc(ctx, func() { from.Close(); to.Close() })
	defer stop()

	var sent atomic.Int64
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				j.ImageBytes(i, sent.Load(), size)
			}
		}
	}()
	_, cerr := io.Copy(countWriter{to.Stdin, &sent}, from.Stdout)
	_ = to.Stdin.Close()
	close(done)
	j.ImageBytes(i, sent.Load(), size)
	serr := from.Wait()
	out, _ := io.ReadAll(to.Stdout)
	lerr := to.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case strings.Contains(from.Stderr(), "@@save-failed"):
		return fmt.Errorf("docker save failed on %s: %s", src.Srv.Name, strings.TrimSpace(strings.ReplaceAll(from.Stderr(), "@@save-failed", "")))
	case serr != nil:
		return fmt.Errorf("reading the image on %s: %w", src.Srv.Name, serr)
	case lerr != nil:
		return fmt.Errorf("docker load failed on %s: %w", dst.Srv.Name, lerr)
	case cerr != nil:
		return fmt.Errorf("the copy was interrupted: %w", cerr)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		j.ImageLog(i, line)
	}
	if img.ID != "" {
		res, err := dst.Conn.Exec(ctx, sshpool.DockerBin(dst.Srv)+" tag "+ops.Quote(img.ID)+" "+ops.Quote(img.Name), 30*time.Second)
		if err == nil && res.Code != 0 {
			err = errors.New(strings.TrimSpace(res.Stderr + res.Stdout))
		}
		if err != nil {
			return fmt.Errorf("naming the image %s on %s: %w", img.Name, dst.Srv.Name, err)
		}
		j.ImageLog(i, "$ docker tag "+short(img.ID)+" "+img.Name)
	}
	return nil
}

type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}
