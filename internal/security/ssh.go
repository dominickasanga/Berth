package security

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file reads the server's SSH log: who tried to get in, from where, and who succeeded.
// Everything is computed on this server; no address is looked up or sent anywhere.

// DefaultAuthLog is where Ubuntu's SSH daemon logs. Berth's user needs read access to it (the adm group).
const DefaultAuthLog = "/var/log/auth.log"

type IPStat struct {
	IP     string    `json:"ip"`
	Failed int       `json:"failed"`
	Users  int       `json:"users"` // different user names tried from this address
	Last   time.Time `json:"last"`
}

type NameStat struct {
	User  string `json:"user"`
	Count int    `json:"count"`
}

// SSHWindow sums up the failed attempts of a period.
type SSHWindow struct {
	Failed   int        `json:"failed"`
	IPs      int        `json:"ips"`
	Users    int        `json:"users"`
	Root     int        `json:"root"` // attempts to log in as root
	TopIPs   []IPStat   `json:"top_ips"`
	TopUsers []NameStat `json:"top_users"`
}

type SSHLogin struct {
	At     time.Time `json:"at"`
	User   string    `json:"user"`
	IP     string    `json:"ip"`
	Method string    `json:"method"`
}

// SSHFlag is a successful login from an address that had failed many times just before: either the
// owner mistyping, or a guessed password.
type SSHFlag struct {
	At     time.Time `json:"at"`
	User   string    `json:"user"`
	IP     string    `json:"ip"`
	Failed int       `json:"failed"`
}

type SSHConfig struct {
	PasswordAuth string   `json:"password_auth"` // yes or no
	RootLogin    string   `json:"root_login"`
	Port         string   `json:"port"`
	Problems     []string `json:"problems"`
}

// SSHReport is the whole picture, refreshed every few minutes.
type SSHReport struct {
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	Error     string    `json:"error,omitempty"`
	Day, Week SSHWindow
	Logins    []SSHLogin `json:"logins"` // the week's successful logins, newest first
	Flagged   []SSHFlag  `json:"flagged"`
	Config    SSHConfig  `json:"config"`
	Fail2ban  string     `json:"fail2ban"` // active, inactive, ... or unknown
}

type sshEvent struct {
	At            time.Time
	User, IP      string
	Port          string
	Invalid, Fail bool // Fail: a failed attempt; otherwise a successful login
	Method        string
}

var (
	isoLine     = regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)) \S+ sshd(?:-session)?(?:\[\d+\])?: (.*)$`)
	classicLine = regexp.MustCompile(`^([A-Z][a-z]{2}\s+\d{1,2} \d\d:\d\d:\d\d) \S+ sshd(?:-session)?(?:\[\d+\])?: (.*)$`)
	failedMsg   = regexp.MustCompile(`^Failed (\S+) for (invalid user )?(\S+) from (\S+) port (\d+)`)
	invalidMsg  = regexp.MustCompile(`^Invalid user (\S*) from (\S+) port (\d+)`)
	acceptedMsg = regexp.MustCompile(`^Accepted (\S+) for (\S+) from (\S+) port (\d+)`)
)

// logTime reads the time of a log line: ISO 8601 (newer Ubuntu) or "Nov 24 12:15:03" (older, no year).
func logTime(line string, now time.Time) (time.Time, string, bool) {
	if m := isoLine.FindStringSubmatch(line); m != nil {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, m[1]); err == nil {
				return t, m[2], true
			}
		}
	}
	if m := classicLine.FindStringSubmatch(line); m != nil {
		t, err := time.ParseInLocation("Jan _2 15:04:05 2006", m[1]+" "+fmt.Sprint(now.Year()), now.Location())
		if err != nil {
			return time.Time{}, "", false
		}
		if t.After(now.Add(24 * time.Hour)) { // a December line read in January
			t = t.AddDate(-1, 0, 0)
		}
		return t, m[2], true
	}
	return time.Time{}, "", false
}

func parseEvents(r io.Reader, now time.Time) []sshEvent {
	var out []sshEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		at, msg, ok := logTime(sc.Text(), now)
		if !ok {
			continue
		}
		switch {
		case failedMsg.MatchString(msg):
			m := failedMsg.FindStringSubmatch(msg)
			out = append(out, sshEvent{At: at, User: m[3], IP: m[4], Port: m[5], Fail: true, Invalid: m[2] != "", Method: m[1]})
		case invalidMsg.MatchString(msg):
			m := invalidMsg.FindStringSubmatch(msg)
			out = append(out, sshEvent{At: at, User: m[1], IP: m[2], Port: m[3], Fail: true, Invalid: true, Method: "invalid"})
		case acceptedMsg.MatchString(msg):
			m := acceptedMsg.FindStringSubmatch(msg)
			out = append(out, sshEvent{At: at, User: m[2], IP: m[3], Port: m[4], Method: m[1]})
		}
	}
	return out
}

// window sums the failed attempts that happened after since.
func window(failed []sshEvent, since time.Time) SSHWindow {
	var w SSHWindow
	ips := map[string]*IPStat{}
	ipUsers := map[string]map[string]bool{}
	users := map[string]int{}
	for _, e := range failed {
		if e.At.Before(since) {
			continue
		}
		w.Failed++
		if e.User == "root" {
			w.Root++
		}
		s := ips[e.IP]
		if s == nil {
			s = &IPStat{IP: e.IP}
			ips[e.IP] = s
			ipUsers[e.IP] = map[string]bool{}
		}
		s.Failed++
		ipUsers[e.IP][e.User] = true
		if e.At.After(s.Last) {
			s.Last = e.At
		}
		users[e.User]++
	}
	for ip, s := range ips {
		s.Users = len(ipUsers[ip])
		w.TopIPs = append(w.TopIPs, *s)
	}
	for u, n := range users {
		w.TopUsers = append(w.TopUsers, NameStat{User: u, Count: n})
	}
	sort.Slice(w.TopIPs, func(i, j int) bool {
		if w.TopIPs[i].Failed != w.TopIPs[j].Failed {
			return w.TopIPs[i].Failed > w.TopIPs[j].Failed
		}
		return w.TopIPs[i].IP < w.TopIPs[j].IP
	})
	sort.Slice(w.TopUsers, func(i, j int) bool {
		if w.TopUsers[i].Count != w.TopUsers[j].Count {
			return w.TopUsers[i].Count > w.TopUsers[j].Count
		}
		return w.TopUsers[i].User < w.TopUsers[j].User
	})
	w.IPs, w.Users = len(w.TopIPs), len(w.TopUsers)
	if len(w.TopIPs) > 10 {
		w.TopIPs = w.TopIPs[:10]
	}
	if len(w.TopUsers) > 10 {
		w.TopUsers = w.TopUsers[:10]
	}
	return w
}

// flagThreshold is how many failures from one address, in the day before a successful login, make it worth a look.
const flagThreshold = 5

// Summarize turns log events into a report. Failed attempts of one connection are counted once per line,
// and the "Invalid user" line that sshd writes next to "Failed password for invalid user" is not counted twice.
func Summarize(events []sshEvent, now time.Time) SSHReport {
	failedKeys := map[string]bool{}
	for _, e := range events {
		if e.Fail && e.Method != "invalid" {
			failedKeys[e.IP+"|"+e.Port+"|"+e.User] = true
		}
	}
	var fails []sshEvent
	var logins []sshEvent
	for _, e := range events {
		switch {
		case e.Fail && e.Method == "invalid" && failedKeys[e.IP+"|"+e.Port+"|"+e.User]:
			// the same attempt is already counted by its "Failed password" line
		case e.Fail:
			fails = append(fails, e)
		default:
			logins = append(logins, e)
		}
	}
	rep := SSHReport{At: now, Day: window(fails, now.Add(-24*time.Hour)), Week: window(fails, now.Add(-7*24*time.Hour))}
	sort.Slice(logins, func(i, j int) bool { return logins[i].At.After(logins[j].At) })
	for _, l := range logins {
		if l.At.Before(now.Add(-7 * 24 * time.Hour)) {
			continue
		}
		if len(rep.Logins) < 15 {
			rep.Logins = append(rep.Logins, SSHLogin{At: l.At, User: l.User, IP: l.IP, Method: l.Method})
		}
		n := 0
		for _, f := range fails {
			if f.IP == l.IP && f.At.Before(l.At) && l.At.Sub(f.At) <= 24*time.Hour {
				n++
			}
		}
		if n >= flagThreshold {
			rep.Flagged = append(rep.Flagged, SSHFlag{At: l.At, User: l.User, IP: l.IP, Failed: n})
		}
	}
	return rep
}

// ReadAuthLog reads the SSH log and the previous (rotated) one, so a week is covered.
func ReadAuthLog(path string, now time.Time) ([]sshEvent, error) {
	var out []sshEvent
	read := false
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if err != nil {
			if p == path {
				return nil, err
			}
			continue
		}
		read = true
		out = append(out, parseEvents(f, now)...)
		f.Close()
	}
	if gz, err := os.Open(path + ".2.gz"); err == nil {
		if zr, err := gzip.NewReader(gz); err == nil {
			out = append(out, parseEvents(zr, now)...)
		}
		gz.Close()
	}
	if !read {
		return nil, fmt.Errorf("no log found")
	}
	return out, nil
}

// ParseSSHDConfig reads sshd's settings from its files, given in the order sshd reads them (the first
// value of a setting wins, and a Match block ends the global part of a file).
func ParseSSHDConfig(files ...string) SSHConfig {
	got := map[string]string{}
	for _, content := range files {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := strings.Fields(line)
			key := strings.ToLower(f[0])
			if key == "match" {
				break
			}
			if len(f) > 1 {
				if _, seen := got[key]; !seen {
					got[key] = strings.ToLower(f[1])
				}
			}
		}
	}
	c := SSHConfig{PasswordAuth: "yes", RootLogin: "prohibit-password", Port: "22"} // sshd's own defaults
	if v := got["passwordauthentication"]; v != "" {
		c.PasswordAuth = v
	}
	if v := got["permitrootlogin"]; v != "" {
		c.RootLogin = v
	}
	if v := got["port"]; v != "" {
		c.Port = v
	}
	if c.PasswordAuth == "yes" {
		c.Problems = append(c.Problems, "Password login is on: anyone on the internet can keep guessing passwords. Allow keys only with PasswordAuthentication no.")
	}
	if c.RootLogin == "yes" {
		c.Problems = append(c.Problems, "Root can log in over SSH with a password. Set PermitRootLogin no (or prohibit-password).")
	}
	return c
}

func readSSHD(dir string) SSHConfig {
	var files []string
	if names, err := filepath.Glob(filepath.Join(dir, "sshd_config.d", "*.conf")); err == nil {
		sort.Strings(names)
		for _, n := range names {
			if b, err := os.ReadFile(n); err == nil {
				files = append(files, string(b))
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "sshd_config"))
	if err != nil {
		return SSHConfig{}
	}
	return ParseSSHDConfig(append(files, string(b))...)
}

func fail2banState(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cctx, "systemctl", "is-active", "fail2ban").Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "unknown"
}

// AccessWatcher keeps the latest SSH report, refreshed every few minutes.
type AccessWatcher struct {
	AuthLog string // DefaultAuthLog unless set
	SSHDir  string // /etc/ssh unless set

	mu     sync.Mutex
	report *SSHReport
}

// Report is the latest SSH report, or nil before the first refresh.
func (w *AccessWatcher) Report() *SSHReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.report
}

// Refresh reads the logs and settings again.
func (w *AccessWatcher) Refresh(ctx context.Context) {
	path, dir, now := w.AuthLog, w.SSHDir, time.Now()
	if path == "" {
		path = DefaultAuthLog
	}
	if dir == "" {
		dir = "/etc/ssh"
	}
	var rep SSHReport
	events, err := ReadAuthLog(path, now)
	if err != nil {
		rep = SSHReport{At: now, Error: fmt.Sprintf("cannot read %s (%v). Berth's user needs read access to it: on Ubuntu, add it to the adm group", path, simplify(err))}
	} else {
		rep = Summarize(events, now)
	}
	rep.Source = path
	rep.Config = readSSHD(dir)
	rep.Fail2ban = fail2banState(ctx)
	w.mu.Lock()
	w.report = &rep
	w.mu.Unlock()
}

func simplify(err error) string {
	if os.IsNotExist(err) {
		return "it does not exist"
	}
	if os.IsPermission(err) {
		return "permission denied"
	}
	return err.Error()
}

// Run refreshes now and then every five minutes.
func (w *AccessWatcher) Run(ctx context.Context) {
	for {
		w.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}
