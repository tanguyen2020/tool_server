package ops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"serverdash/internal/sshpool"
	"serverdash/internal/store"
)

// ------------------------------------------------------------------ unused images and volumes

// UnusedImage is an image no container (running or stopped) was created from.
type UnusedImage struct {
	ID      string   `json:"id"`   // sha256:…
	Tags    []string `json:"tags"` // repo:tag; empty for a dangling image
	Size    string   `json:"size"`
	Bytes   int64    `json:"bytes"`
	Created string   `json:"created"` // "3 weeks ago"
}

// UnusedVolume is a volume no container (running or stopped) mounts.
type UnusedVolume struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Project   string `json:"project"`   // compose project label, if any
	Anonymous bool   `json:"anonymous"` // 64-hex name docker made up
	Size      string `json:"size"`      // "" when docker system df -v gave nothing
	Bytes     int64  `json:"bytes"`
}

type Unused struct {
	Images  []UnusedImage  `json:"images"`
	Volumes []UnusedVolume `json:"volumes"`
}

var (
	imageIDRe  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	volumeRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
	anonVolRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	dockerSize = regexp.MustCompile(`^([\d.]+)\s*([a-zA-Z]*)$`)
)

// FindUnused lists the images and volumes no container uses. withSizes also runs `docker system df -v`
// (slow on big volumes) to get each volume's size.
func FindUnused(ctx context.Context, conn *sshpool.Conn, srv store.Server, withSizes bool) (Unused, error) {
	d := sshpool.DockerBin(srv)
	script := fmt.Sprintf(`echo @@IMAGES; %[1]s image ls --no-trunc --format '{{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}' || exit 1
echo @@USED; %[1]s ps -aq --no-trunc | xargs -r %[1]s container inspect -f '{{.Image}}' 2>/dev/null
echo @@VOLUMES; %[1]s volume ls -f dangling=true --format '{{.Name}}\t{{.Driver}}\t{{.Label "com.docker.compose.project"}}' || exit 1
`, d)
	if withSizes {
		script += "echo @@SIZES; " + d + " system df -v 2>/dev/null\n"
	}
	script += "exit 0"
	out, err := run(ctx, conn, script, "list unused images and volumes", 3*time.Minute)
	if err != nil {
		return Unused{}, err
	}
	return parseUnused(out), nil
}

func parseUnused(out string) Unused {
	sections := map[string][]string{}
	cur := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "@@") {
			cur = l[2:]
			continue
		}
		sections[cur] = append(sections[cur], l)
	}

	used := map[string]bool{}
	for _, l := range sections["USED"] {
		if l = strings.TrimSpace(l); l != "" {
			used[l] = true
		}
	}

	res := Unused{Images: []UnusedImage{}, Volumes: []UnusedVolume{}}
	byID := map[string]int{}
	for _, l := range sections["IMAGES"] {
		f := strings.Split(l, "\t")
		if len(f) < 5 || !imageIDRe.MatchString(f[0]) || used[f[0]] {
			continue
		}
		i, ok := byID[f[0]]
		if !ok {
			i = len(res.Images)
			byID[f[0]] = i
			res.Images = append(res.Images, UnusedImage{ID: f[0], Tags: []string{}, Size: f[3], Bytes: parseDockerSize(f[3]), Created: f[4]})
		}
		if f[1] != "<none>" && f[2] != "<none>" {
			res.Images[i].Tags = append(res.Images[i].Tags, f[1]+":"+f[2])
		}
	}
	sort.SliceStable(res.Images, func(a, b int) bool { return res.Images[a].Bytes > res.Images[b].Bytes })

	sizes := volumeSizes(sections["SIZES"])
	for _, l := range sections["VOLUMES"] {
		f := strings.Split(l, "\t")
		if len(f) < 2 || !volumeRe.MatchString(f[0]) {
			continue
		}
		v := UnusedVolume{Name: f[0], Driver: f[1], Anonymous: anonVolRe.MatchString(f[0]), Size: sizes[f[0]]}
		if len(f) > 2 {
			v.Project = strings.TrimSpace(f[2])
		}
		v.Bytes = parseDockerSize(v.Size)
		res.Volumes = append(res.Volumes, v)
	}
	sort.SliceStable(res.Volumes, func(a, b int) bool { return res.Volumes[a].Bytes > res.Volumes[b].Bytes })
	return res
}

// volumeSizes reads the "Local Volumes space usage" table of `docker system df -v`:
// VOLUME NAME / LINKS / SIZE, one volume per line, ending at a blank line.
func volumeSizes(lines []string) map[string]string {
	sizes := map[string]string{}
	in := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "Local Volumes space usage"):
			in = true
		case !in || strings.HasPrefix(l, "VOLUME NAME"):
		case strings.TrimSpace(l) == "":
			if len(sizes) > 0 {
				return sizes
			}
		default:
			if f := strings.Fields(l); len(f) >= 3 {
				sizes[f[0]] = f[len(f)-1]
			}
		}
	}
	return sizes
}

// parseDockerSize reads docker's human sizes ("1.2GB", "512kB", "0B"), which use powers of 1000.
func parseDockerSize(s string) int64 {
	m := dockerSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"": 1, "b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12, "pb": 1e15}[strings.ToLower(m[2])]
	return int64(n * mult)
}

// RemoveResult counts what RemoveUnused deleted.
type RemoveResult struct {
	Images  int      `json:"images"`
	Volumes int      `json:"volumes"`
	Skipped int      `json:"skipped"` // gone, or used by a container since the list was made
	Errors  []string `json:"errors"`
}

// RemoveUnused deletes the given images (by ID) and volumes (by name). The list is made again first and
// only what is still unused is removed, so a container created in the meantime keeps its image and volumes.
func RemoveUnused(ctx context.Context, conn *sshpool.Conn, srv store.Server, imageIDs, volumes []string) (RemoveResult, error) {
	res := RemoveResult{Errors: []string{}}
	for _, id := range imageIDs {
		if !imageIDRe.MatchString(id) {
			return res, errors.New("invalid image ID")
		}
	}
	for _, v := range volumes {
		if !volumeRe.MatchString(v) {
			return res, errors.New("invalid volume name")
		}
	}
	if len(imageIDs)+len(volumes) == 0 {
		return res, errors.New("nothing selected")
	}
	cur, err := FindUnused(ctx, conn, srv, false)
	if err != nil {
		return res, err
	}

	type item struct{ kind, name string }
	var items []item
	d := sshpool.DockerBin(srv)
	// r runs one removal and prints @@OK or @@ERR with docker's message on one line.
	var sb strings.Builder
	sb.WriteString(`r() { if o=$("$@" 2>&1); then echo @@OK; else printf '@@ERR %s\n' "$(printf '%s' "$o" | tr '\n' ' ')"; fi; }` + "\n")

	images := map[string]UnusedImage{}
	for _, im := range cur.Images {
		images[im.ID] = im
	}
	for _, id := range imageIDs {
		im, ok := images[id]
		if !ok {
			res.Skipped++
			continue
		}
		// By tag: `image rm <id>` refuses an image with several tags. The last tag removed deletes the image.
		refs := []string{Quote(id)}
		name := id[7:19]
		if len(im.Tags) > 0 {
			refs = refs[:0]
			for _, t := range im.Tags {
				refs = append(refs, Quote(t))
			}
			name = im.Tags[0]
		}
		items = append(items, item{"image", name})
		sb.WriteString("r " + d + " image rm " + strings.Join(refs, " ") + "\n")
	}

	vols := map[string]bool{}
	for _, v := range cur.Volumes {
		vols[v.Name] = true
	}
	for _, v := range volumes {
		if !vols[v] {
			res.Skipped++
			continue
		}
		items = append(items, item{"volume", v})
		sb.WriteString("r " + d + " volume rm " + Quote(v) + "\n")
	}
	if len(items) == 0 {
		return res, nil
	}

	out, err := run(ctx, conn, sb.String(), "remove unused images and volumes", 10*time.Minute)
	if err != nil {
		return res, err
	}
	i := 0
	for _, l := range strings.Split(out, "\n") {
		if i >= len(items) || !strings.HasPrefix(l, "@@") {
			continue
		}
		it := items[i]
		i++
		switch {
		case l == "@@OK" && it.kind == "image":
			res.Images++
		case l == "@@OK":
			res.Volumes++
		default:
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %s", it.kind, it.name, strings.TrimSpace(strings.TrimPrefix(l, "@@ERR"))))
		}
	}
	return res, nil
}
