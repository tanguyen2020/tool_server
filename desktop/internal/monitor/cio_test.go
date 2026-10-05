package monitor

import (
	"strings"
	"testing"
	"time"
)

var (
	idA = strings.Repeat("a", 64)
	idB = strings.Repeat("b", 64)
)

func TestParseCgroupIOAndNet(t *testing.T) {
	io := parseCgroupIO([]string{
		"#/sys/fs/cgroup/system.slice/docker-" + idA + ".scope/io.stat",
		"254:0 rbytes=1000 wbytes=2000 rios=1 wios=2 dbytes=0 dios=0",
		"8:16 rbytes=500 wbytes=0 rios=1 wios=0 dbytes=0 dios=0",
		"#/sys/fs/cgroup/docker/" + idB + "/io.stat",
		"254:0 rbytes=7 wbytes=9",
	})
	if io[idA] != [2]float64{1500, 2000} || io[idB] != [2]float64{7, 9} {
		t.Fatalf("wrong io: %v", io)
	}
	net := parseContainerNet([]string{
		"#" + idA,
		"Inter-|   Receive                                                |  Transmit",
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed",
		"    lo:  999 0 0 0 0 0 0 0  999 0 0 0 0 0 0 0",
		"  eth0: 1000 0 0 0 0 0 0 0  4000 0 0 0 0 0 0 0",
		"  eth1:  500 0 0 0 0 0 0 0   100 0 0 0 0 0 0 0",
	})
	if net[idA] != [2]float64{1500, 4100} {
		t.Fatalf("wrong net: %v", net)
	}
}

func TestParseEvents(t *testing.T) {
	ev := parseEvents([]string{
		`{"status":"die","id":"x","Type":"container","Action":"die","Actor":{"ID":"x","Attributes":{"exitCode":"137","name":"api"}},"time":1759550000,"timeNano":1759550000123456789}`,
		`{"Type":"container","Action":"health_status: unhealthy","Actor":{"Attributes":{"name":"keycloak"}},"time":1759550010}`,
		`{"Type":"container","Action":"kill","Actor":{"Attributes":{"name":"web","signal":"15"}},"time":1759550020}`,
		`not json`,
	})
	if len(ev) != 3 {
		t.Fatalf("3 events expected, got %+v", ev)
	}
	if ev[0].T != 1759550000123 || ev[0].Action != "die" || ev[0].Detail != "137" || ev[0].Container != "api" {
		t.Fatalf("wrong die event: %+v", ev[0])
	}
	if ev[1].Action != "health" || ev[1].Detail != "unhealthy" || ev[1].T != 1759550010000 {
		t.Fatalf("wrong health event: %+v", ev[1])
	}
	if ev[2].Detail != "15" {
		t.Fatalf("wrong kill event: %+v", ev[2])
	}
}

func TestContainerRates(t *testing.T) {
	mk := func() *Docker {
		return &Docker{Containers: []Container{
			{ID: idA, Name: "a", State: "running", counters: ctrCounters{net: [2]float64{1e6, 1e6}, blk: [2]float64{0, 0}, netSrc: 's', blkSrc: 's'}},
			{ID: idB, Name: "b", State: "running", counters: ctrCounters{net: [2]float64{5e6, 5e6}, blk: [2]float64{1e6, 1e6}, netSrc: 's', blkSrc: 's'}},
		}}
	}
	t0 := time.Now()
	prev := &raw{at: t0}
	containerRates(mk(), prev, nil,
		map[string][2]float64{idA: {1000, 2000}},
		map[string][2]float64{idA: {10000, 20000}}, nil)
	cur := &raw{at: t0.Add(2 * time.Second)}
	d := mk()
	containerRates(d, cur, prev,
		map[string][2]float64{idA: {3000, 2000}},
		map[string][2]float64{idA: {14000, 20000}}, nil)
	a, b := d.Containers[0], d.Containers[1]
	if a.BlkRead == nil || *a.BlkRead != 1000 || *a.BlkWrite != 0 || *a.NetRx != 2000 || *a.NetTx != 0 {
		t.Fatalf("exact rates expected for a: %v %v %v %v", a.BlkRead, a.BlkWrite, a.NetRx, a.NetTx)
	}
	// b only has docker stats counters: rates from the fallback source (unchanged -> 0).
	if b.NetRx == nil || *b.NetRx != 0 || b.BlkRead == nil {
		t.Fatalf("fallback rates expected for b: %+v", b)
	}
	// A counter reset (container restarted) must not produce a rate.
	cur2 := &raw{at: t0.Add(4 * time.Second)}
	d2 := mk()
	containerRates(d2, cur2, cur, map[string][2]float64{idA: {10, 10}}, map[string][2]float64{idA: {10, 10}}, nil)
	if d2.Containers[0].BlkRead != nil || d2.Containers[0].NetRx != nil {
		t.Fatalf("reset counters must give no rate: %+v", d2.Containers[0])
	}
}

func TestScriptOptions(t *testing.T) {
	base := script("docker", scriptOpts{})
	if strings.Contains(base, "@@INSPECT") || strings.Contains(base, "@@CNET") || !strings.Contains(base, "@@CIO") {
		t.Fatalf("base script must only read cgroup io")
	}
	full := script("sudo -n docker", scriptOpts{inspect: true, eventsSince: 1759550000, netPIDs: map[string]int{idA: 4242}})
	for _, want := range []string{"@@INSPECT", "sudo -n docker inspect", "--since 1759550000", "event=health_status", "cat /proc/4242/net/dev", "#" + idA} {
		if !strings.Contains(full, want) {
			t.Fatalf("script must contain %q", want)
		}
	}
	if !strings.Contains(script("docker", scriptOpts{inspect: true}), "--since $(( $(date +%s) - 3600 ))") {
		t.Fatalf("first events fetch must cover the last hour")
	}
}
