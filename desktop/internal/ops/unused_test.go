package ops

import (
	"strings"
	"testing"
)

func TestParseUnused(t *testing.T) {
	idA := "sha256:" + strings.Repeat("a", 64) // used by a container
	idB := "sha256:" + strings.Repeat("b", 64) // unused, two tags
	idC := "sha256:" + strings.Repeat("c", 64) // dangling
	anon := strings.Repeat("f", 64)
	out := "@@IMAGES\n" +
		idA + "\tnginx\tlatest\t187MB\t2 weeks ago\n" +
		idB + "\tmyapp\tv1\t1.2GB\t3 months ago\n" +
		idB + "\tmyapp\tlatest\t1.2GB\t3 months ago\n" +
		idC + "\t<none>\t<none>\t45.3MB\t5 days ago\n" +
		"@@USED\n" + idA + "\n" + idA + "\n" +
		"@@VOLUMES\n" +
		"shop_dbdata\tlocal\tshop\n" +
		anon + "\tlocal\t\n" +
		"@@SIZES\n" +
		"Images space usage:\n\nREPOSITORY   TAG   IMAGE ID   CREATED   SIZE   SHARED SIZE   UNIQUE SIZE   CONTAINERS\nnginx   latest   aaa   2 weeks ago   187MB   0B   187MB   1\n\n" +
		"Local Volumes space usage:\n\nVOLUME NAME                                                        LINKS     SIZE\n" +
		"shop_dbdata                                                        0         2.5GB\n" +
		anon + "   0         0B\n" +
		"used_vol                                                           1         10MB\n\n" +
		"Build cache usage: 0B\n"

	u := parseUnused(out)
	if len(u.Images) != 2 {
		t.Fatalf("want 2 unused images, got %+v", u.Images)
	}
	if b := u.Images[0]; b.ID != idB || len(b.Tags) != 2 || b.Tags[0] != "myapp:v1" || b.Bytes != 1.2e9 {
		t.Errorf("largest first, tags grouped: %+v", b)
	}
	if c := u.Images[1]; c.ID != idC || len(c.Tags) != 0 || c.Created != "5 days ago" {
		t.Errorf("dangling: %+v", c)
	}

	if len(u.Volumes) != 2 {
		t.Fatalf("want 2 volumes, got %+v", u.Volumes)
	}
	if v := u.Volumes[0]; v.Name != "shop_dbdata" || v.Project != "shop" || v.Anonymous || v.Size != "2.5GB" || v.Bytes != 2.5e9 {
		t.Errorf("named volume: %+v", v)
	}
	if v := u.Volumes[1]; !v.Anonymous || v.Size != "0B" || v.Project != "" {
		t.Errorf("anonymous volume: %+v", v)
	}
}

func TestParseUnusedWithoutSizes(t *testing.T) {
	u := parseUnused("@@IMAGES\n@@USED\n@@VOLUMES\ndata\tlocal\t\n")
	if len(u.Images) != 0 || len(u.Volumes) != 1 || u.Volumes[0].Size != "" {
		t.Errorf("%+v", u)
	}
}

func TestParseDockerSize(t *testing.T) {
	for in, want := range map[string]int64{"0B": 0, "512kB": 512e3, "1.5GB": 1.5e9, "45.3MB": 45.3e6, "": 0, "N/A": 0} {
		if got := parseDockerSize(in); got != want {
			t.Errorf("%q: got %d, want %d", in, got, want)
		}
	}
}
