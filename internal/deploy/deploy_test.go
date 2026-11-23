package deploy

import (
	"testing"

	"github.com/dominickasanga/berth/internal/store"
)

func TestValidateApp(t *testing.T) {
	ok := store.App{Name: "x", RepoURL: "https://github.com/a/b", Branch: "main", Subfolder: "go-api", ComposeFile: "docker-compose.yml"}
	if err := ValidateApp(ok); err != nil {
		t.Fatalf("valid app rejected: %v", err)
	}
	bad := map[string]func(*store.App){
		"option-like repo":   func(a *store.App) { a.RepoURL = "--upload-pack=evil" },
		"option-like branch": func(a *store.App) { a.Branch = "-x" },
		"dotdot branch":      func(a *store.App) { a.Branch = "a/../b" },
		"subfolder escape":   func(a *store.App) { a.Subfolder = "../../etc" },
		"absolute compose":   func(a *store.App) { a.ComposeFile = "/etc/passwd" },
		"bad predeploy":      func(a *store.App) { a.PredeploySvc = "x; rm -rf /" },
	}
	for name, mutate := range bad {
		a := ok
		mutate(&a)
		if err := ValidateApp(a); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestQuoteEnv(t *testing.T) {
	for in, want := range map[string]string{
		"plain":    `'plain'`,
		"a$b":      `'a$b'`,
		`it's "x"`: `"it's \"x\""`,
	} {
		if got := quoteEnv(in); got != want {
			t.Errorf("quoteEnv(%q)=%s want %s", in, got, want)
		}
	}
}

func TestSplitRef(t *testing.T) {
	for in, want := range map[string][2]string{
		"berth-demo-web":             {"berth-demo-web", "latest"},
		"nginx:1.27":                 {"nginx", "1.27"},
		"localhost:5000/team/app":    {"localhost:5000/team/app", "latest"},
		"localhost:5000/team/app:v2": {"localhost:5000/team/app", "v2"},
	} {
		repo, tag := splitRef(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q)=%s,%s want %v", in, repo, tag, want)
		}
	}
}
