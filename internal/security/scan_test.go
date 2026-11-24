package security

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

func TestContainerIssues(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	raw := `[
	 {"Name":"/app-web-1","Image":"sha256:aaa","State":{"Status":"running"},"Config":{"User":""},
	  "HostConfig":{"NetworkMode":"host","PortBindings":{}},"Mounts":[]},
	 {"Name":"/app-api-1","Image":"sha256:bbb","State":{"Status":"running"},"Config":{"User":"1001:1001"},
	  "HostConfig":{"Privileged":true,"NetworkMode":"app_default","CapAdd":["SYS_ADMIN","NET_RAW"],"PidMode":"host",
	   "PortBindings":{"8001/tcp":[{"HostIp":"","HostPort":"8001"}],"9000/tcp":[{"HostIp":"127.0.0.1","HostPort":"9000"}]}},
	  "Mounts":[{"Source":"/var/run/docker.sock","Destination":"/var/run/docker.sock"}]},
	 {"Name":"/app-old-1","Image":"sha256:ccc","State":{"Status":"exited"},"Config":{"User":""},"HostConfig":{"Privileged":true},"Mounts":[]}
	]`
	built := map[string]time.Time{"sha256:aaa": now.Add(-400 * 24 * time.Hour), "sha256:bbb": now.Add(-10 * 24 * time.Hour)}
	issues, err := ContainerIssues([]byte(raw), built, now)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string][]string{}
	for _, i := range issues {
		by[i.Container] = append(by[i.Container], i.Severity+":"+i.Title)
	}
	web := strings.Join(by["app-web-1"], "|")
	api := strings.Join(by["app-api-1"], "|")
	for _, want := range []string{"medium:Uses the host network", "medium:Runs as root", "low:Old image"} {
		if !strings.Contains(web, want) {
			t.Errorf("web lacks %q: %s", want, web)
		}
	}
	for _, want := range []string{"high:Runs privileged", "high:Can control Docker", "high:Extra Linux capability SYS_ADMIN", "low:Extra Linux capability NET_RAW", "high:Shares the server's process list", "low:Published on all network interfaces"} {
		if !strings.Contains(api, want) {
			t.Errorf("api lacks %q: %s", want, api)
		}
	}
	if strings.Contains(api, "Runs as root") || strings.Contains(api, "Old image") || strings.Contains(api, "9000") {
		t.Errorf("api runs as 1001, is recent, and only publishes 8001 on all interfaces: %s", api)
	}
	if len(by["app-old-1"]) != 0 {
		t.Errorf("a stopped container is ignored: %v", by["app-old-1"])
	}
	if issues[0].Severity != "high" || issues[len(issues)-1].Severity == "high" {
		t.Errorf("most serious first: %+v", issues)
	}
	if _, err := ContainerIssues([]byte("not json"), nil, now); err == nil {
		t.Error("garbage is an error")
	}
}

func TestClassify(t *testing.T) {
	for p, want := range map[string]bool{".env": true, "go-api/.env.local.bak": true, ".env.production": true, "x/.env.example": false, "main.go": false,
		"cookies.txt": true, "keys/server.pem": true, "db/backup.dump": true, "migrations/001.sql": false, "id_rsa": true, "notes.bak": true, "README.md": false} {
		if got := classify(p) != ""; got != want {
			t.Errorf("%s: flagged=%v, want %v", p, got, want)
		}
	}
}

func TestRepoSecretsFindsTrackedAndHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	run("init", "-q")
	write("main.go", "package main")
	write(".env.example", "A=")
	write("cookies.txt", "session=abc")
	write("go-api/.env.local.bak", "KEY=do-not-print-me")
	write("firebase.json", `{"type":"service_account","private_key":"-----BEGIN-----x","client_email":"a@b.iam"}`)
	write("settings.json", `{"theme":"dark"}`)
	write("old/.env", "X=1")
	run("add", "-A")
	run("commit", "-q", "-m", "first")
	run("rm", "-q", "-r", "old")
	run("commit", "-q", "-m", "remove")

	got, err := RepoSecrets(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var desc []string
	for _, f := range got {
		desc = append(desc, f.Path+"|"+map[bool]string{true: "tracked", false: "history"}[f.Tracked])
		if strings.Contains(f.Reason, "do-not-print-me") || strings.Contains(f.Reason, "BEGIN") {
			t.Errorf("a secret value leaked into the reason: %q", f.Reason)
		}
	}
	want := "cookies.txt|tracked,firebase.json|tracked,go-api/.env.local.bak|tracked,old/.env|history"
	if strings.Join(desc, ",") != want {
		t.Fatalf("got %v\nwant %s", desc, want)
	}
	if _, err := RepoSecrets(context.Background(), t.TempDir()); err == nil {
		t.Error("a folder that is not a repository is an error")
	}
}

func TestScanAppCombinesTheChecks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	vc, _, _ := fakeDB(t)
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(dir, "go-api"), 0o755)
	os.WriteFile(filepath.Join(dir, "go-api", "go.mod"), []byte("module app\nrequire example.com/old v1.2.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("PASSWORD=never-shown"), 0o644)
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "x")

	r := &Runner{Dir: func(store.App) string { return dir }, ProjectName: func(store.App) string { return "no-such-project-for-the-test" }, Vuln: vc}
	res := r.ScanApp(context.Background(), store.App{Name: "demo"})
	if res.VulnError != "" || len(res.GoMods) != 1 || res.GoMods[0].GoMod != "go-api/go.mod" || res.VulnerableModules() != 1 {
		t.Fatalf("vulnerabilities: %+v", res)
	}
	if f := res.GoMods[0].Findings[0]; f.Module != "example.com/old" || f.UpdateTo() != "1.5.0" || f.NoFix() != 0 {
		t.Fatalf("the finding says what to update to: %+v", f)
	}
	if res.SecretError != "" || len(res.Secrets) != 1 || res.Secrets[0].Path != ".env" || !res.Secrets[0].Tracked {
		t.Fatalf("secrets: %+v", res.Secrets)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), "never-shown") {
		t.Fatal("a secret value reached the stored result")
	}
	// the folder is not there yet (an app that was never deployed): the checks say so instead of looking clean
	r.Dir = func(store.App) string { return filepath.Join(dir, "missing") }
	gone := r.ScanApp(context.Background(), store.App{Name: "demo"})
	if !strings.Contains(gone.VulnError, "not found") || !strings.Contains(gone.SecretError, "not found") {
		t.Fatalf("a missing folder is reported: %+v", gone)
	}
}
