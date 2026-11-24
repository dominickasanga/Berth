package monitor

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestParseCPUMemoryNetwork(t *testing.T) {
	total, idle, err := parseCPU("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4 5 6 7 8 9 10\n")
	if err != nil || total != 1000 || idle != 850 { // idle includes iowait
		t.Fatalf("cpu: %v %v %v", total, idle, err)
	}
	if _, _, err := parseCPU("nonsense"); err == nil {
		t.Fatal("a broken /proc/stat must be an error")
	}
	mt, ma, err := parseMeminfo("MemTotal:       16000000 kB\nMemFree:  100 kB\nMemAvailable:    4000000 kB\n")
	if err != nil || !near(mt, 16000000*1024) || !near(ma, 4000000*1024) {
		t.Fatalf("meminfo: %v %v %v", mt, ma, err)
	}
	if _, _, err := parseMeminfo("MemTotal: 1 kB\n"); err == nil {
		t.Fatal("MemAvailable is required: free memory alone would count the cache as used")
	}
	rx, tx := parseNetDev(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 999999 10 0 0 0 0 0 0 999999 10 0 0 0 0 0 0
  ens3: 1000 10 0 0 0 0 0 0 2000 10 0 0 0 0 0 0
docker0: 5555 10 0 0 0 0 0 0 6666 10 0 0 0 0 0 0
vethabc: 777 10 0 0 0 0 0 0 888 10 0 0 0 0 0 0
br-1234: 111 10 0 0 0 0 0 0 222 10 0 0 0 0 0 0
  eth1: 30 1 0 0 0 0 0 0 40 1 0 0 0 0 0 0
`)
	if rx != 1030 || tx != 2040 {
		t.Fatalf("only the network cards count, not lo, docker or veth: rx=%v tx=%v", rx, tx)
	}
}

func TestRealMountsAndSizes(t *testing.T) {
	got := realMounts(`/dev/sda1 / ext4 rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/sdb1 /mnt/blockvolume xfs rw 0 0
/dev/sdb1 /mnt/other xfs rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
/dev/loop1 /snap/core/1 squashfs ro 0 0
/dev/sda2 /var/lib/docker/volumes ext4 rw 0 0
`)
	if strings.Join(got, ",") != "/,/mnt/blockvolume" {
		t.Fatalf("one entry per real disk, without docker's or snap's: %v", got)
	}
	for in, want := range map[string]float64{"159.2MiB": 159.2 * (1 << 20), "1.5GiB": 1.5 * (1 << 30), "512kB": 512e3, "0B": 0, "--": 0} {
		if g := parseSize(in); !near(g, want) {
			t.Errorf("parseSize(%q)=%v want %v", in, g, want)
		}
	}
	if d, err := statDisk("/"); err != nil || d.Size <= 0 || d.Used <= 0 || d.pct() <= 0 || d.pct() > 100 {
		t.Fatalf("statDisk: %+v %v", d, err)
	}
}

func TestContainerSummary(t *testing.T) {
	cs := parseInspect(`aaaa1111|/berth-sb-api-api-1|running|0|healthy
bbbb2222|/berth-sb-api-worker-1|restarting|9|
cccc3333|/berth-sb-api-old-1|exited|1|
dddd4444|/berth-sb-api-web-1|running|2|unhealthy
`)
	stats := parseStats("aaaa|12.5%|159.2MiB / 7.7GiB\ndddd|1.5%|50MiB / 7.7GiB\n")
	r := summarize(cs, stats)
	m := r.Metrics
	if !r.Up || m["containers_total"] != 4 || m["containers_running"] != 2 || m["containers_restarting"] != 1 || m["containers_unhealthy"] != 1 || m["restarts"] != 12 {
		t.Fatalf("counts: %+v", m)
	}
	if !near(m["cpu_pct"], 14) || !near(m["mem_bytes"], (159.2+50)*(1<<20)) {
		t.Fatalf("usage adds up over running containers: %+v", m)
	}
	if len(r.Tables) != 1 || len(r.Tables[0].Rows) != 4 || r.Tables[0].Rows[0][1] != "running (healthy)" {
		t.Fatalf("table: %+v", r.Tables)
	}
	// stopped on purpose: no containers running is not an error and raises nothing
	stopped := summarize(parseInspect("eeee5555|/berth-x-1|exited|0|\n"), nil)
	if !stopped.Up || len(Conditions("container", nil, stopped, 0)) != 0 {
		t.Fatalf("a stopped app must not alert: %+v", stopped)
	}
	if keys := Conditions("container", nil, r, 0); len(keys) != 2 || keys[0].Key != "restarting" || keys[0].Severity != "critical" || keys[1].Key != "unhealthy" {
		t.Fatalf("conditions: %+v", keys)
	}
}

func TestSystemConditions(t *testing.T) {
	res := Result{Up: true, Metrics: map[string]float64{"disk_pct": 91, "mem_pct": 95, "cpu_pct": 100}, Info: map[string]string{"disk_worst": "/mnt/blockvolume"}}
	cs := Conditions("system", nil, res, 0)
	if len(cs) != 2 || cs[0].Key != "disk" || !strings.Contains(cs[0].Message, "/mnt/blockvolume") || cs[1].Key != "memory" {
		t.Fatalf("disk and memory alert, a busy CPU alone does not: %+v", cs)
	}
	res.Metrics["disk_pct"], res.Metrics["mem_pct"] = 40, 50
	if cs := Conditions("system", nil, res, 0); len(cs) != 0 {
		t.Fatalf("healthy: %+v", cs)
	}
	if got := Format("bytes_s", 2048); got != "2.0 KiB/s" {
		t.Fatalf("rate format: %q", got)
	}
	for _, k := range KindNames() {
		if k == "system" || k == "container" {
			t.Fatalf("%s must not be offered in the add form", k)
		}
	}
}

func TestCollectSystemOnLinux(t *testing.T) {
	if !SystemSupported() {
		t.Skip("needs /proc")
	}
	first := collectSystem(context.Background(), Spec{Kind: "system"}, Prev{}, time.Now())
	if !first.Up || first.Metrics["mem_pct"] <= 0 || first.Metrics["disk_pct"] <= 0 || first.Info["cpus"] == "" {
		t.Fatalf("first check: %+v", first)
	}
	if _, ok := first.Metrics["cpu_pct"]; ok {
		t.Fatal("a CPU percentage needs two checks")
	}
	time.Sleep(300 * time.Millisecond)
	second := collectSystem(context.Background(), Spec{Kind: "system"}, Prev{Raw: first.Raw, At: time.Now().Add(-time.Second)}, time.Now())
	if _, ok := second.Metrics["cpu_pct"]; !ok {
		t.Fatalf("second check should have CPU: %+v", second.Metrics)
	}
	if _, ok := second.Metrics["net_rx_bytes_s"]; !ok {
		t.Fatalf("second check should have network rates: %+v", second.Metrics)
	}
}
