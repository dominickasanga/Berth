package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

// This file measures the server Berth runs on (CPU, memory, disks, network, load), read from /proc,
// and the containers of each app, read from the docker CLI. Both feed the same samples, charts and
// alerts as the supporting services: "system" is one internal service called "server", "container"
// is one internal service per app.

// SystemSupported reports whether this machine can be measured (Linux: it needs /proc).
func SystemSupported() bool { return runtime.GOOS == "linux" }

// cpuTimes returns the total and idle jiffies from the first line of /proc/stat.
func parseCPU(stat string) (total, idle float64, err error) {
	line, _, _ := strings.Cut(stat, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, fmt.Errorf("unexpected /proc/stat")
	}
	for i, s := range f[1:] {
		v, e := strconv.ParseFloat(s, 64)
		if e != nil {
			return 0, 0, e
		}
		if i >= 8 { // guest time is already inside user time
			break
		}
		total += v
		if i == 3 || i == 4 { // idle and iowait
			idle += v
		}
	}
	return total, idle, nil
}

// parseMeminfo returns MemTotal and MemAvailable in bytes.
func parseMeminfo(s string) (total, avail float64, err error) {
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, e := strconv.ParseFloat(f[0], 64)
		if e != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	if total <= 0 || avail <= 0 {
		return 0, 0, fmt.Errorf("MemTotal or MemAvailable missing from /proc/meminfo")
	}
	return total, avail, nil
}

// physicalNIC is false for loopback and the virtual interfaces Docker creates, so the traffic
// counted is what really crosses the server's network cards.
func physicalNIC(name string) bool {
	if name == "lo" {
		return false
	}
	for _, p := range []string{"docker", "veth", "br-", "virbr", "cni", "flannel", "tun", "tap"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// parseNetDev sums received and sent bytes over the physical interfaces of /proc/net/dev.
func parseNetDev(s string) (rx, tx float64) {
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if len(f) < 9 || !physicalNIC(name) {
			continue
		}
		r, e1 := strconv.ParseFloat(f[0], 64)
		t, e2 := strconv.ParseFloat(f[8], 64)
		if e1 == nil && e2 == nil {
			rx += r
			tx += t
		}
	}
	return rx, tx
}

var diskTypes = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "vfat": true}

// realMounts lists the mount points of /proc/mounts that sit on a disk, once per device.
func realMounts(mounts string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !diskTypes[f[2]] || seen[f[0]] || strings.HasPrefix(f[1], "/var/lib/docker") || strings.HasPrefix(f[1], "/snap") {
			continue
		}
		seen[f[0]] = true
		out = append(out, strings.ReplaceAll(f[1], `\040`, " "))
	}
	sort.Strings(out)
	return out
}

type diskUse struct {
	Mount            string
	Size, Used, Free float64
}

func (d diskUse) pct() float64 {
	if d.Size <= 0 {
		return 0
	}
	return 100 * d.Used / d.Size
}

func statDisk(mount string) (diskUse, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil {
		return diskUse{}, err
	}
	bs := float64(st.Bsize)
	d := diskUse{Mount: mount, Size: float64(st.Blocks) * bs, Free: float64(st.Bavail) * bs}
	d.Used = float64(st.Blocks-st.Bfree) * bs
	return d, nil
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// collectSystem measures the server. Param "mounts" (comma-separated) overrides the automatic list of disks.
func collectSystem(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	if !SystemSupported() {
		return fail(fmt.Errorf("server metrics need Linux (/proc)"))
	}
	r := newResult()
	r.Up = true

	if total, idle, err := parseCPU(readFile("/proc/stat")); err == nil {
		r.Raw["cpu_total"], r.Raw["cpu_idle"] = total, idle
		if pt, ok := prev.Raw["cpu_total"]; ok && total > pt {
			r.Metrics["cpu_pct"] = clampPct(100 * (1 - (idle-prev.Raw["cpu_idle"])/(total-pt)))
		}
	}
	if total, avail, err := parseMeminfo(readFile("/proc/meminfo")); err == nil {
		r.Metrics["mem_pct"] = clampPct(100 * (total - avail) / total)
		r.Metrics["mem_used_bytes"] = total - avail
		r.Info["memory"] = humanBytes(total)
	}
	if f := strings.Fields(readFile("/proc/loadavg")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			r.Metrics["load1"] = v
		}
	}
	if n := countCPUs(readFile("/proc/cpuinfo")); n > 0 {
		r.Info["cpus"] = strconv.Itoa(n)
	}
	if f := strings.Fields(readFile("/proc/uptime")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			r.Info["uptime"] = humanDuration(v)
		}
	}
	rx, tx := parseNetDev(readFile("/proc/net/dev"))
	r.Raw["net_rx"], r.Raw["net_tx"] = rx, tx
	if v, ok := rate(prev, r.Raw, "net_rx", now); ok {
		r.Metrics["net_rx_bytes_s"] = v
	}
	if v, ok := rate(prev, r.Raw, "net_tx", now); ok {
		r.Metrics["net_tx_bytes_s"] = v
	}

	mounts := realMounts(readFile("/proc/mounts"))
	if m := strings.TrimSpace(sp.Params["mounts"]); m != "" {
		mounts = splitList(m)
	}
	disks := store.Table{Title: "Disks", Columns: []string{"Mount", "Size", "Used", "Free", "Use"}}
	worst, worstPct := "", -1.0
	for _, m := range mounts {
		d, err := statDisk(m)
		if err != nil || d.Size <= 0 {
			continue
		}
		disks.Rows = append(disks.Rows, []string{m, humanBytes(d.Size), humanBytes(d.Used), humanBytes(d.Free), fmt.Sprintf("%.0f%%", d.pct())})
		if d.pct() > worstPct {
			worst, worstPct = m, d.pct()
		}
	}
	if worstPct >= 0 {
		r.Metrics["disk_pct"] = worstPct
		r.Info["disk_worst"] = worst
		r.Tables = append(r.Tables, disks)
	}
	return r
}

func clampPct(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}

func countCPUs(cpuinfo string) int {
	n := 0
	for _, line := range strings.Split(cpuinfo, "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}

// ---- containers ----

// dockerContainer is what the metrics need to know about one container.
type dockerContainer struct {
	ID, Name, State, Health string
	Restarts                int
	CPU, Mem                float64
}

// parseSize reads docker's "159.2MiB" / "1.5GiB" / "512kB" into bytes.
func parseSize(s string) float64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err != nil {
				return 0
			}
			return v * u.mult
		}
	}
	return 0
}

// parseStats reads `docker stats --format '{{.ID}}|{{.CPUPerc}}|{{.MemUsage}}'` lines.
func parseStats(out string) map[string]dockerContainer {
	m := map[string]dockerContainer{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 3 {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(f[1]), "%"), 64)
		used, _, _ := strings.Cut(f[2], "/")
		m[f[0]] = dockerContainer{ID: f[0], CPU: cpu, Mem: parseSize(used)}
	}
	return m
}

// parseInspect reads `docker inspect --format '{{.Id}}|{{.Name}}|{{.State.Status}}|{{.RestartCount}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}'`.
func parseInspect(out string) []dockerContainer {
	var cs []dockerContainer
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 5 {
			continue
		}
		n, _ := strconv.Atoi(f[3])
		cs = append(cs, dockerContainer{ID: f[0], Name: strings.TrimPrefix(f[1], "/"), State: f[2], Restarts: n, Health: f[4]})
	}
	return cs
}

func docker(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w", args[0], err)
	}
	return string(out), nil
}

// summarize turns the containers of one app into the app's numbers. Containers that are not running
// have no CPU or memory, and an app with none running is not an error (it may be stopped on purpose).
func summarize(cs []dockerContainer, stats map[string]dockerContainer) Result {
	r := newResult()
	r.Up = true
	t := store.Table{Title: "Containers", Columns: []string{"Container", "State", "Restarts", "CPU", "Memory"}}
	var running, restarting, unhealthy, restarts int
	var cpu, mem float64
	for _, c := range cs {
		st := stats[c.ID]
		for id, s := range stats { // docker stats prints the short id; inspect gives the long one
			if strings.HasPrefix(c.ID, id) {
				st = s
			}
		}
		state := c.State
		if c.Health != "" && c.State == "running" {
			state += " (" + c.Health + ")"
		}
		t.Rows = append(t.Rows, []string{c.Name, state, strconv.Itoa(c.Restarts), fmt.Sprintf("%.1f%%", st.CPU), humanBytes(st.Mem)})
		restarts += c.Restarts
		switch {
		case c.State == "running":
			running++
			cpu += st.CPU
			mem += st.Mem
		case c.State == "restarting":
			restarting++
		}
		if c.Health == "unhealthy" {
			unhealthy++
		}
	}
	r.Metrics["containers_running"] = float64(running)
	r.Metrics["containers_total"] = float64(len(cs))
	r.Metrics["containers_restarting"] = float64(restarting)
	r.Metrics["containers_unhealthy"] = float64(unhealthy)
	r.Metrics["restarts"] = float64(restarts)
	r.Metrics["cpu_pct"] = cpu
	r.Metrics["mem_bytes"] = mem
	if len(t.Rows) > 0 {
		r.Tables = append(r.Tables, t)
	}
	return r
}

// collectContainers measures the containers of one compose project (param "project").
func collectContainers(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	project := strings.TrimSpace(sp.Params["project"])
	if project == "" {
		return fail(fmt.Errorf("no compose project is set"))
	}
	ids, err := docker(ctx, "ps", "-a", "-q", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return fail(err)
	}
	idList := strings.Fields(ids)
	if len(idList) == 0 {
		r := summarize(nil, nil)
		r.Info["state"] = "no containers"
		return r
	}
	insp, err := docker(ctx, append([]string{"inspect", "--format", "{{.Id}}|{{.Name}}|{{.State.Status}}|{{.RestartCount}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}"}, idList...)...)
	if err != nil {
		return fail(err)
	}
	stats, err := docker(ctx, append([]string{"stats", "--no-stream", "--format", "{{.ID}}|{{.CPUPerc}}|{{.MemUsage}}"}, idList...)...)
	if err != nil {
		return fail(err)
	}
	return summarize(parseInspect(insp), parseStats(stats))
}
