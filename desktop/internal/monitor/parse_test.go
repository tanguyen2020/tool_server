package monitor

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func sample(busy, idle int, rx int) string {
	return strings.Join([]string{
		"@@HOST", "deb-test",
		"@@OS", "Debian GNU/Linux 12 (bookworm)",
		"@@KERNEL", "6.1.0-25-amd64",
		"@@UPTIME", "266400.50 1000.00",
		"@@LOAD", "0.52 0.40 0.31 1/200 1234",
		"@@CPUMODEL", " Intel(R) Xeon(R) CPU",
		"@@STAT",
		"cpu  " + strconv.Itoa(busy*2) + " 0 0 " + strconv.Itoa(idle*2) + " 0 0 0 0 0 0",
		"cpu0 " + strconv.Itoa(busy) + " 0 0 " + strconv.Itoa(idle) + " 0 0 0 0 0 0",
		"cpu1 " + strconv.Itoa(busy) + " 0 0 " + strconv.Itoa(idle) + " 0 0 0 0 0 0",
		"@@MEM", "MemTotal:        8000000 kB", "MemAvailable:    2000000 kB", "SwapTotal:       0 kB", "SwapFree:        0 kB",
		"@@DISK", "Filesystem 1-blocks Used Available Capacity Mounted on",
		"/dev/sda1 100000 25000 75000 25% /",
		"/dev/sda2 1000 10 990 1% /boot/efi",
		"@@NET", "Inter-|   Receive", " face |bytes",
		"    lo: 999 0 0 0 0 0 0 0 999 0 0 0 0 0 0 0",
		"  eth0: " + strconv.Itoa(rx) + " 0 0 0 0 0 0 0 500 0 0 0 0 0 0 0",
		"veth12: 99999 0 0 0 0 0 0 0 99999 0 0 0 0 0 0 0",
		"@@DOCKER_PS",
		`{"ID":"aaa","Names":"web","Image":"nginx","State":"running","Status":"Up 3 hours","Labels":"com.docker.compose.project=shop,com.docker.compose.service=web"}`,
		`{"ID":"ccc","Names":"redis","Image":"redis","State":"exited","Status":"Exited (0) 2 days ago","Labels":""}`,
		`{"ID":"bbb","Names":"api","Image":"node","State":"running","Status":"Up 1 hour","Labels":"com.docker.compose.project=shop"}`,
		"@@DOCKER_STATS",
		`{"ID":"aaa","CPUPerc":"12.50%","MemUsage":"100MiB / 2GiB","MemPerc":"4.88%","NetIO":"1kB / 2kB","BlockIO":"0B / 0B","PIDs":"7"}`,
		"",
	}, "\n")
}

func TestParse(t *testing.T) {
	first, raw1 := parse("id1", "srv", sample(100, 900, 1000), nil)
	if first.Host.CPU != nil {
		t.Fatalf("first sample must not have CPU%%, got %v", *first.Host.CPU)
	}
	raw1.at = raw1.at.Add(-2 * time.Second)
	snap, _ := parse("id1", "srv", sample(150, 950, 3000), raw1)
	h := snap.Host
	if h.Cores != 2 || h.CPU == nil || *h.CPU != 50 {
		t.Fatalf("wrong CPU: cores=%d cpu=%v", h.Cores, h.CPU)
	}
	if *h.PerCore[0] != 50 || *h.PerCore[1] != 50 {
		t.Fatalf("wrong per-core: %v %v", *h.PerCore[0], *h.PerCore[1])
	}
	if h.Mem.Pct != 75 || h.Mem.Total != 8000000*1024 {
		t.Fatalf("wrong RAM: %+v", h.Mem)
	}
	if len(h.Disks) != 1 || h.Disks[0].Mount != "/" || h.Disks[0].Pct != 25 {
		t.Fatalf("wrong disks: %+v", h.Disks)
	}
	if h.Net.RX == nil || *h.Net.RX < 900 || *h.Net.RX > 1100 {
		t.Fatalf("wrong net rx (excluding lo/veth, ~1000 B/s): %v", h.Net.RX)
	}
	if h.Load != [3]float64{0.52, 0.40, 0.31} || h.Uptime != 266400.5 || h.OS != "Debian GNU/Linux 12 (bookworm)" {
		t.Fatalf("wrong host info: %+v", h)
	}
	d := snap.Docker
	if !d.Available || d.Total != 3 || d.Running != 2 {
		t.Fatalf("wrong docker: %+v", d)
	}
	names := []string{d.Containers[0].Name, d.Containers[1].Name, d.Containers[2].Name}
	if strings.Join(names, ",") != "api,web,redis" {
		t.Fatalf("wrong container order (projects first, standalone last): %v", names)
	}
	web := d.Containers[1]
	if *web.CPU != 12.5 || *web.MemUsed != 100*(1<<20) || *web.MemLimit != 2*(1<<30) || *web.PIDs != 7 || web.Project != "shop" {
		t.Fatalf("wrong container stats: %+v", web)
	}
	if d.Containers[0].CPU != nil {
		t.Fatalf("api has no stats so CPU must be nil")
	}
}

func TestDockerError(t *testing.T) {
	d := parseDocker([]string{"permission denied while trying to connect to the Docker daemon socket"}, nil)
	if d.Available || !strings.Contains(d.Error, "permission denied") {
		t.Fatalf("docker error must be reported: %+v", d)
	}
}
