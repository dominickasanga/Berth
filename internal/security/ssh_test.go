package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var sshNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

const classicLog = `Nov 24 10:00:01 erp sshd[100]: Invalid user admin from 203.0.113.9 port 40001
Nov 24 10:00:02 erp sshd[100]: Failed password for invalid user admin from 203.0.113.9 port 40001 ssh2
Nov 24 10:00:05 erp sshd[100]: Failed password for invalid user admin from 203.0.113.9 port 40001 ssh2
Nov 24 10:01:00 erp sshd[101]: Failed password for root from 203.0.113.9 port 40002 ssh2
Nov 24 10:02:00 erp sshd[102]: Invalid user oracle from 198.51.100.4 port 50000
Nov 24 10:03:00 erp sshd[103]: Failed password for ubuntu from 192.0.2.7 port 1 ssh2
Nov 23 08:00:00 erp sshd[104]: Failed password for root from 203.0.113.9 port 40010 ssh2
Nov 10 08:00:00 erp sshd[105]: Failed password for root from 203.0.113.50 port 1 ssh2
Nov 24 11:00:00 erp sshd[106]: Accepted publickey for ubuntu from 192.0.2.50 port 22222 ssh2: RSA SHA256:abc
Nov 24 11:30:00 erp sshd[107]: Accepted password for ubuntu from 203.0.113.9 port 40099 ssh2
Nov 24 11:31:00 erp CRON[9]: pam_unix(cron:session): session opened
this is not a log line
`

func TestSummarizeClassicLog(t *testing.T) {
	events := parseEvents(strings.NewReader(classicLog), sshNow)
	rep := Summarize(events, sshNow)
	// 24 h: admin x2 (the Invalid user line of the same connection is not counted again) + root + oracle + ubuntu = 5
	if rep.Day.Failed != 5 || rep.Day.IPs != 3 || rep.Day.Root != 1 {
		t.Fatalf("day: %+v", rep.Day)
	}
	// 7 d adds the root attempt of yesterday; the one from the 10th is older than a week
	if rep.Week.Failed != 6 || rep.Week.TopIPs[0].IP != "203.0.113.9" || rep.Week.TopIPs[0].Failed != 4 || rep.Week.TopIPs[0].Users != 2 {
		t.Fatalf("week: %+v", rep.Week)
	}
	if len(rep.Week.TopUsers) != 4 || rep.Week.TopUsers[0].User != "admin" || rep.Week.TopUsers[0].Count != 2 || rep.Week.TopUsers[1].User != "root" || rep.Week.TopUsers[1].Count != 2 {
		t.Fatalf("most tried user: %+v", rep.Week.TopUsers)
	}
	if len(rep.Logins) != 2 || rep.Logins[0].User != "ubuntu" || rep.Logins[0].IP != "203.0.113.9" || rep.Logins[0].Method != "password" || rep.Logins[1].Method != "publickey" {
		t.Fatalf("logins, newest first: %+v", rep.Logins)
	}
	// 3 failures in the day before the password login: below the threshold, so not flagged
	if len(rep.Flagged) != 0 {
		t.Fatalf("flagged: %+v", rep.Flagged)
	}
}

func TestLoginAfterManyFailuresIsFlagged(t *testing.T) {
	line := func(min int, what string) string {
		return sshNow.Add(-time.Duration(min)*time.Minute).Format("Jan _2 15:04:05") + " erp sshd[1]: " + what + "\n"
	}
	var log strings.Builder
	for i := 0; i < 5; i++ { // five failures from 203.0.113.9, then a login from it
		log.WriteString(line(60-i, "Failed password for ubuntu from 203.0.113.9 port 10 ssh2"))
	}
	for i := 0; i < 4; i++ { // four failures from 198.51.100.1, then a login from it
		log.WriteString(line(60-i, "Failed password for ubuntu from 198.51.100.1 port 11 ssh2"))
	}
	log.WriteString(line(30, "Accepted password for ubuntu from 203.0.113.9 port 12 ssh2"))
	log.WriteString(line(30, "Accepted password for ubuntu from 198.51.100.1 port 13 ssh2"))
	log.WriteString(line(20, "Accepted password for ubuntu from 192.0.2.9 port 14 ssh2")) // never failed
	rep := Summarize(parseEvents(strings.NewReader(log.String()), sshNow), sshNow)
	if len(rep.Flagged) != 1 || rep.Flagged[0].IP != "203.0.113.9" || rep.Flagged[0].Failed != 5 || rep.Flagged[0].User != "ubuntu" {
		t.Fatalf("only the address with five failures before its login: %+v", rep.Flagged)
	}
}

func TestIsoTimestampsAndNewDaemonName(t *testing.T) {
	log := `2026-11-24T11:00:00.123456+00:00 erp sshd-session[1]: Failed password for invalid user test from 203.0.113.1 port 5 ssh2
2026-11-24T11:00:01+00:00 erp sshd[2]: Invalid user  from 203.0.113.2 port 6
2026-11-23T11:00:00Z erp sshd[3]: Accepted publickey for ubuntu from 192.0.2.1 port 7 ssh2
`
	rep := Summarize(parseEvents(strings.NewReader(log), sshNow), sshNow)
	if rep.Day.Failed != 2 || rep.Day.IPs != 2 || len(rep.Logins) != 1 {
		t.Fatalf("%+v", rep)
	}
	// a December line read in January belongs to last year
	jan := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	at, _, ok := logTime("Dec 31 23:59:00 erp sshd[1]: x", jan)
	if !ok || at.Year() != 2026 {
		t.Fatalf("year rollover: %v %v", at, ok)
	}
}

func TestParseSSHDConfig(t *testing.T) {
	// files in sshd's order: the .d files first, then the main file; the first value wins
	cloud := "PasswordAuthentication no\n"
	main := "Port 2222\nPermitRootLogin yes\nPasswordAuthentication yes\nMatch User bob\n  PasswordAuthentication yes\n"
	c := ParseSSHDConfig(cloud, main)
	if c.PasswordAuth != "no" || c.RootLogin != "yes" || c.Port != "2222" || len(c.Problems) != 1 || !strings.Contains(c.Problems[0], "Root") {
		t.Fatalf("%+v", c)
	}
	d := ParseSSHDConfig("# nothing set\n")
	if d.PasswordAuth != "yes" || d.RootLogin != "prohibit-password" || d.Port != "22" || len(d.Problems) != 1 || !strings.Contains(d.Problems[0], "Password login is on") {
		t.Fatalf("sshd's defaults allow passwords: %+v", d)
	}
}

func TestAccessWatcherReadsFilesAndSaysWhenItCannot(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "auth.log")
	os.WriteFile(log, []byte(time.Now().Format("Jan _2 15:04:05")+" erp sshd[1]: Failed password for root from 203.0.113.9 port 1 ssh2\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "ssh", "sshd_config.d"), 0o755)
	os.WriteFile(filepath.Join(dir, "ssh", "sshd_config"), []byte("PasswordAuthentication yes\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ssh", "sshd_config.d", "50-cloud-init.conf"), []byte("PasswordAuthentication no\n"), 0o644)
	w := &AccessWatcher{AuthLog: log, SSHDir: filepath.Join(dir, "ssh")}
	if w.Report() != nil {
		t.Fatal("no report before the first refresh")
	}
	w.Refresh(t.Context())
	r := w.Report()
	if r.Error != "" || r.Day.Failed != 1 || r.Config.PasswordAuth != "no" || r.Source != log {
		t.Fatalf("%+v", r)
	}
	w2 := &AccessWatcher{AuthLog: filepath.Join(dir, "nope.log"), SSHDir: dir}
	w2.Refresh(t.Context())
	if r := w2.Report(); !strings.Contains(r.Error, "adm group") || !strings.Contains(r.Error, "does not exist") {
		t.Fatalf("a log that cannot be read says so and how to fix it: %q", r.Error)
	}
}
