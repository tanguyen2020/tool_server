package monitor

import (
	"testing"
	"time"
)

func TestPSIAndSockstat(t *testing.T) {
	prev := parsePSI([]string{
		"cpu some avg10=0.00 avg60=0.00 avg300=0.00 total=1000000",
		"memory some avg10=0.00 avg60=0.00 avg300=0.00 total=500000",
		"memory full avg10=0.00 avg60=0.00 avg300=0.00 total=100000",
		"io some avg10=0.00 avg60=0.00 avg300=0.00 total=2000000",
		"io full avg10=0.00 avg60=0.00 avg300=0.00 total=1000000",
	})
	cur := parsePSI([]string{
		"cpu some avg10=1.00 avg60=0.50 avg300=0.10 total=1250000", // +0.25 s in 5 s -> 5%
		"memory some avg10=0.00 avg60=0.00 avg300=0.00 total=500000",
		"memory full avg10=0.00 avg60=0.00 avg300=0.00 total=100000",
		"io some avg10=0.00 avg60=0.00 avg300=0.00 total=3000000", // +1 s -> 20%
		"io full avg10=0.00 avg60=0.00 avg300=0.00 total=1500000", // +0.5 s -> 10%
	})
	p := psiRates(prev, cur, 5)
	if p == nil || *p.CPU != 5 || *p.Mem != 0 || *p.IO != 20 || *p.IOFull != 10 {
		t.Fatalf("psi: %+v", p)
	}
	if psiRates(nil, map[string]float64{}, 5) != nil {
		t.Error("no PSI on the server must give nil")
	}

	tcp := parseSockstat([]string{"sockets: used 300", "TCP: inuse 25 orphan 0 tw 12 alloc 40 mem 3", "UDP: inuse 4 mem 2", "TCP6: inuse 7"})
	if tcp == nil || tcp.InUse != 32 || tcp.TimeWait != 12 {
		t.Fatalf("tcp: %+v", tcp)
	}
}

func TestDiskAwaitAndThrottling(t *testing.T) {
	// diskstats: reads(3) .. read ms(6) .. writes(7) .. write ms(10) .. io ms(12)
	prev := parseDiskstats([]string{"   8       0 sda 100 0 800 50 100 0 800 150 0 1000 0"})
	cur := parseDiskstats([]string{"   8       0 sda 150 0 1200 150 150 0 1200 450 0 1500 0"})
	if c := cur["sda"]; c.qticks != 600 || c.ops != 300 {
		t.Fatalf("counters: %+v", c)
	}
	// 100 requests, 400 ms spent -> 4 ms each
	if await := (cur["sda"].qticks - prev["sda"].qticks) / (cur["sda"].ops - prev["sda"].ops); await != 4 {
		t.Fatalf("await %v", await)
	}

	cpu := parseCgroupCPU([]string{"#/sys/fs/cgroup/system.slice/docker-" + idA + ".scope/cpu.stat", "nr_periods 1000", "nr_throttled 100"})
	if v := cpu[idA]; v != [2]float64{1000, 100} {
		t.Fatalf("cgroup cpu: %v", v)
	}
	mk := func() *Docker {
		return &Docker{Containers: []Container{{ID: idA, Name: "api", State: "running"}}}
	}
	t0 := time.Now()
	r0 := &raw{at: t0}
	containerRates(mk(), r0, nil, nil, nil, cpu)
	r1 := &raw{at: t0.Add(5 * time.Second)}
	d := mk()
	containerRates(d, r1, r0, nil, nil, map[string][2]float64{idA: {1050, 125}}) // 25 of 50 periods throttled
	if th := d.Containers[0].Throttled; th == nil || *th != 50 {
		t.Fatalf("throttled: %v", th)
	}
	// No CPU limit: periods do not move, no figure.
	r2 := &raw{at: t0.Add(10 * time.Second)}
	d2 := mk()
	containerRates(d2, r2, r1, nil, nil, map[string][2]float64{idA: {1050, 125}})
	if d2.Containers[0].Throttled != nil {
		t.Fatal("no periods must give no throttling value")
	}
}
