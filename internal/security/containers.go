package security

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// ContainerIssue is one risky setting of a running container.
type ContainerIssue struct {
	Container string `json:"container"`
	Severity  string `json:"severity"` // high, medium, low
	Title     string `json:"title"`
	Detail    string `json:"detail"`
}

type inspected struct {
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	State  struct{ Status string }
	Config struct {
		User  string
		Image string
	}
	HostConfig struct {
		Privileged   bool
		NetworkMode  string
		PidMode      string
		CapAdd       []string
		PortBindings map[string][]struct{ HostIp, HostPort string }
	}
	Mounts []struct{ Source, Destination string }
}

const staleImageDays = 180

var severityOrder = map[string]int{"high": 0, "medium": 1, "low": 2}

// ContainerIssues reads the output of `docker inspect` for the containers of one app. imageBuilt maps an
// image id to when it was built (nil skips the age check). Containers that are not running are ignored.
func ContainerIssues(inspectJSON []byte, imageBuilt map[string]time.Time, now time.Time) ([]ContainerIssue, error) {
	var cs []inspected
	if err := json.Unmarshal(inspectJSON, &cs); err != nil {
		return nil, fmt.Errorf("cannot read docker's answer: %w", err)
	}
	var out []ContainerIssue
	for _, c := range cs {
		if c.State.Status != "running" && c.State.Status != "restarting" {
			continue
		}
		name := strings.TrimPrefix(c.Name, "/")
		add := func(sev, title, detail string) {
			out = append(out, ContainerIssue{Container: name, Severity: sev, Title: title, Detail: detail})
		}
		h := c.HostConfig
		if h.Privileged {
			add("high", "Runs privileged", "The container has almost the same access as root on the server. Remove `privileged: true` unless the app truly needs it.")
		}
		for _, m := range c.Mounts {
			if strings.HasSuffix(m.Source, "docker.sock") {
				add("high", "Can control Docker", "The Docker socket is mounted, so anything running in this container can start containers and take over the server.")
			}
		}
		for _, cap := range h.CapAdd {
			switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(cap), "CAP_")) {
			case "SYS_ADMIN", "ALL", "SYS_PTRACE", "SYS_MODULE":
				add("high", "Extra Linux capability "+cap, "This capability gives wide access to the host. Check that the app really needs it.")
			default:
				add("low", "Extra Linux capability "+cap, "Added on top of the defaults.")
			}
		}
		if h.PidMode == "host" {
			add("high", "Shares the server's process list", "pid mode is host, so the container can see and signal every process on the server.")
		}
		if h.NetworkMode == "host" {
			add("medium", "Uses the host network", "No network isolation: every port the app opens is opened on the server itself, and it can reach services listening only on localhost.")
		}
		if u := c.Config.User; u == "" || u == "0" || u == "root" || strings.HasPrefix(u, "0:") || strings.HasPrefix(u, "root:") {
			add("medium", "Runs as root", "A flaw in the app would give an attacker root inside the container. Set a non-root `user:` where the image allows it.")
		}
		var ports []string
		for port, binds := range h.PortBindings {
			for _, b := range binds {
				if b.HostIp == "" || b.HostIp == "0.0.0.0" || b.HostIp == "::" {
					ports = append(ports, b.HostPort+" ("+strings.TrimSuffix(port, "/tcp")+")")
				}
			}
		}
		if len(ports) > 0 {
			sort.Strings(ports)
			add("low", "Published on all network interfaces", "Port "+strings.Join(ports, ", ")+": reachable from outside the server wherever the firewall allows it. Bind to 127.0.0.1 if only local services need it.")
		}
		if built, ok := imageBuilt[c.Image]; ok && now.Sub(built) > staleImageDays*24*time.Hour {
			add("low", "Old image", fmt.Sprintf("Built %d days ago, so it misses recent security updates of its base image. Redeploy to rebuild it.", int(now.Sub(built).Hours()/24)))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return severityOrder[out[i].Severity] < severityOrder[out[j].Severity] })
	return out, nil
}

func docker(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w", args[0], err)
	}
	return string(out), nil
}

// CheckContainers inspects the containers of a compose project.
func CheckContainers(ctx context.Context, project string) ([]ContainerIssue, int, error) {
	ids, err := docker(ctx, "ps", "-q", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return nil, 0, err
	}
	idList := strings.Fields(ids)
	if len(idList) == 0 {
		return nil, 0, nil
	}
	raw, err := docker(ctx, append([]string{"inspect"}, idList...)...)
	if err != nil {
		return nil, 0, err
	}
	var imgs []inspected
	json.Unmarshal([]byte(raw), &imgs)
	var imageIDs []string
	seen := map[string]bool{}
	for _, c := range imgs {
		if !seen[c.Image] {
			seen[c.Image] = true
			imageIDs = append(imageIDs, c.Image)
		}
	}
	built := map[string]time.Time{}
	if out, err := docker(ctx, append([]string{"image", "inspect", "--format", "{{.Id}}|{{.Created}}"}, imageIDs...)...); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if id, created, ok := strings.Cut(strings.TrimSpace(line), "|"); ok {
				if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
					built[id] = t
				}
			}
		}
	}
	issues, err := ContainerIssues([]byte(raw), built, time.Now())
	return issues, len(idList), err
}
