package deploy

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dominickasanga/berth/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// repos builds origin (bare), the user's live folder (a clone) and a second clone used to push new commits.
func repos(t *testing.T) (live, pusher string) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run()
	pusher = filepath.Join(root, "pusher")
	exec.Command("git", "clone", "-q", origin, pusher).Run()
	git(t, pusher, "checkout", "-q", "-b", "main")
	write(t, filepath.Join(pusher, "app.txt"), "v1\n")
	write(t, filepath.Join(pusher, "compose.yml"), "services: {}\n")
	git(t, pusher, "add", "-A")
	git(t, pusher, "commit", "-qm", "v1")
	git(t, pusher, "push", "-q", "origin", "main")
	live = filepath.Join(root, "live")
	exec.Command("git", "clone", "-q", "-b", "main", origin, live).Run()
	return live, pusher
}

func push(t *testing.T, pusher, file, content string) {
	write(t, filepath.Join(pusher, file), content)
	git(t, pusher, "add", "-A")
	git(t, pusher, "commit", "-qm", "change "+file)
	git(t, pusher, "push", "-q", "origin", "main")
}

func run(cmd *exec.Cmd) error { return cmd.Run() }

func TestUpdateExistingFastForwardKeepsHandEdits(t *testing.T) {
	live, pusher := repos(t)
	write(t, filepath.Join(live, ".env"), "SECRET=edited-by-hand\n") // untracked, like most .env files
	write(t, filepath.Join(live, "notes.txt"), "mine\n")
	push(t, pusher, "app.txt", "v2\n")

	if err := updateExisting(live, "main", run, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(live, "app.txt")); got != "v2\n" {
		t.Fatalf("new commit was not pulled: %q", got)
	}
	if got := read(t, filepath.Join(live, ".env")); got != "SECRET=edited-by-hand\n" {
		t.Fatalf(".env was changed: %q", got)
	}
	if read(t, filepath.Join(live, "notes.txt")) != "mine\n" {
		t.Fatal("untracked file was touched")
	}
}

func TestUpdateExistingNeverDestroysLocalChanges(t *testing.T) {
	live, pusher := repos(t)
	// the user edited a tracked file that upstream also changes
	write(t, filepath.Join(live, "app.txt"), "my local edit\n")
	push(t, pusher, "app.txt", "v2\n")
	if err := updateExisting(live, "main", run, io.Discard); err == nil {
		t.Fatal("expected the update to be refused")
	}
	if got := read(t, filepath.Join(live, "app.txt")); got != "my local edit\n" {
		t.Fatalf("local edit was lost: %q", got)
	}
}

func TestUpdateExistingRefusesDivergedBranchAndWrongBranch(t *testing.T) {
	live, pusher := repos(t)
	write(t, filepath.Join(live, "local.txt"), "x\n")
	git(t, live, "add", "-A")
	git(t, live, "commit", "-qm", "local commit")
	local := git(t, live, "rev-parse", "HEAD")
	push(t, pusher, "app.txt", "v2\n")
	if err := updateExisting(live, "main", run, io.Discard); err == nil {
		t.Fatal("diverged branch must not be merged or reset")
	}
	if git(t, live, "rev-parse", "HEAD") != local {
		t.Fatal("HEAD moved on a diverged branch")
	}
	git(t, live, "checkout", "-q", "-b", "feature")
	err := updateExisting(live, "main", run, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "never switches branches") {
		t.Fatalf("wrong branch should be refused, got %v", err)
	}
	if git(t, live, "rev-parse", "--abbrev-ref", "HEAD") != "feature" {
		t.Fatal("Berth switched the user's branch")
	}
	if err := updateExisting(t.TempDir(), "main", run, io.Discard); err == nil {
		t.Fatal("a folder without .git must be refused")
	}
}

func TestEnvArgsForExistingFolder(t *testing.T) {
	data, dir := t.TempDir(), t.TempDir()
	e := &Engine{DataDir: data}
	app := store.App{Name: "x", SourceMode: "existing"}
	if got := e.envArgs(app, dir); len(got) != 0 {
		t.Fatalf("no env files => compose defaults, got %v", got)
	}
	write(t, filepath.Join(dir, ".env"), "A=1\n")
	if got := e.envArgs(app, dir); len(got) != 2 || got[1] != filepath.Join(dir, ".env") {
		t.Fatalf("folder .env should be used, got %v", got)
	}
	os.MkdirAll(filepath.Join(data, "env"), 0o700)
	write(t, e.envFile(app), "")
	if got := e.envArgs(app, dir); len(got) != 2 {
		t.Fatalf("an empty Berth env file must not hide the folder's .env, got %v", got)
	}
	write(t, e.envFile(app), "A='2'\n")
	got := e.envArgs(app, dir)
	if len(got) != 4 || got[3] != e.envFile(app) {
		t.Fatalf("Berth variables must come last so they override, got %v", got)
	}
}

func TestValidateExistingMode(t *testing.T) {
	ok := store.App{RepoURL: "https://x/y", Branch: "main", ComposeFile: "docker-compose.yml", SourceMode: "existing", SourcePath: "/var/www/app"}
	if err := ValidateApp(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*store.App){
		"relative path": func(a *store.App) { a.SourcePath = "app" },
		"dotdot path":   func(a *store.App) { a.SourcePath = "/var/www/../etc" },
		"unknown mode":  func(a *store.App) { a.SourceMode = "weird" },
	} {
		a := ok
		mut(&a)
		if ValidateApp(a) == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestBuildOverrideHoldsReferencesOnly(t *testing.T) {
	got := buildOverride([]string{"api", "migrate"}, []string{"DATABASE_URL", "TOKEN"})
	want := "services:\n  api:\n    environment:\n      DATABASE_URL: \"${DATABASE_URL}\"\n      TOKEN: \"${TOKEN}\"\n  migrate:\n"
	if !strings.Contains(got, want) {
		t.Fatalf("unexpected override:\n%s", got)
	}
	if strings.Count(got, "${DATABASE_URL}") != 2 || strings.Contains(got, "postgres") {
		t.Fatalf("override must hold references for every service and no values:\n%s", got)
	}
}
