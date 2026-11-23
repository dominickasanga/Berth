package discover

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dominickasanga/berth/config"
)

var expected = []config.ExpectedApp{
	{Name: "kite360-api", Repo: "https://github.com/dominickasanga/Kite360-API", FolderNames: []string{"kite360-api"}},
}

func gitFolder(t *testing.T, parent, name, remote, branch string) {
	t.Helper()
	dir := filepath.Join(parent, name, ".git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n"
	if remote != "" {
		cfg += "[remote \"origin\"]\n\turl = " + remote + "\n"
	}
	os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o644)
	os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644)
}

func TestNormalizeTreatsUrlFormsAsEqual(t *testing.T) {
	want := "github.com/dominickasanga/kite360"
	for _, u := range []string{
		"https://github.com/dominickasanga/kite360.git",
		"git@github.com:dominickasanga/kite360.git",
		"ssh://git@github.com/dominickasanga/kite360",
		"https://tok:x@github.com/DominicKasanga/Kite360/",
	} {
		if got := Normalize(u); got != want {
			t.Errorf("Normalize(%q)=%q want %q", u, got, want)
		}
	}
}

func TestStripCredentials(t *testing.T) {
	if got := StripCredentials("https://user:ghp_secret@github.com/a/b.git"); got != "https://github.com/a/b.git" {
		t.Fatalf("token not stripped: %s", got)
	}
	if got := StripCredentials("git@github.com:a/b.git"); got != "git@github.com:a/b.git" {
		t.Fatalf("scp url changed: %s", got)
	}
}

func TestScanFindsByRemoteAndByName(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	gitFolder(t, a, "Kite360-API", "git@github.com:dominickasanga/Kite360-API.git", "main")         // folder + remote
	gitFolder(t, b, "renamed-copy", "https://github.com/dominickasanga/Kite360-API.git", "release") // remote only
	gitFolder(t, b, "kite360-api", "https://github.com/someone/else.git", "main")                   // name only, wrong remote
	gitFolder(t, a, "unrelated", "https://github.com/x/y.git", "main")
	os.MkdirAll(filepath.Join(b, "kite360-api-plain"), 0o755) // no match

	found, notes := Scan([]string{a, b, filepath.Join(a, "missing")}, expected)
	got := found["kite360-api"]
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d: %+v", len(got), got)
	}
	if len(notes) != 1 || notes[0].Problem != "does not exist on this machine" {
		t.Fatalf("missing path not reported: %+v", notes)
	}
	byFolder := map[string]Candidate{}
	for _, c := range got {
		byFolder[c.Folder] = c
	}
	if c := byFolder["Kite360-API"]; !c.RemoteMatch || !c.NameMatch || c.Warning() != "" {
		t.Errorf("exact copy misclassified: %+v", c)
	}
	if c := byFolder["renamed-copy"]; !c.RemoteMatch || c.NameMatch || c.Branch != "release" {
		t.Errorf("renamed copy misclassified: %+v", c)
	}
	if c := byFolder["kite360-api"]; c.RemoteMatch || c.Warning() == "" {
		t.Errorf("wrong-remote copy should warn: %+v", c)
	}
}

func TestResolve(t *testing.T) {
	one := []Candidate{{Path: "/a"}}
	two := []Candidate{{Path: "/a"}, {Path: "/b"}}
	if c, need := Resolve(one, ""); c == nil || need {
		t.Error("a single copy should be used automatically")
	}
	if c, need := Resolve(two, ""); c != nil || !need {
		t.Error("two copies with no flag must ask the user")
	}
	if c, need := Resolve(two, "/b"); c == nil || c.Path != "/b" || need {
		t.Error("flagged copy should win")
	}
	if _, need := Resolve(two, "/gone"); !need {
		t.Error("a flag pointing at a vanished copy must ask again")
	}
	if c, need := Resolve(nil, ""); c != nil || need {
		t.Error("nothing found is not a choice")
	}
}

func TestScanOthersFindsComposeFoldersNotExpected(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	mk("Other App/docker-compose.yml", "services: {}")
	mk("Other App/.git/config", "[remote \"origin\"]\n\turl = git@github.com:me/other.git\n")
	mk("Other App/.git/HEAD", "ref: refs/heads/dev\n")
	mk("deep/deployments/docker/docker-compose.prod.yml", "services: {}")
	mk("deep/deployments/docker/docker-compose.yml", "services: {}")
	mk("kite360-api/go-api/docker-compose.yml", "services: {}")      // expected app: not "other"
	mk("nodejs/node_modules/lib/docker-compose.yml", "services: {}") // inside node_modules: ignored
	mk("too/a/b/c/docker-compose.yml", "services: {}")               // three folders down: ignored
	mk("plain/README.md", "x")                                       // no compose file
	mk("registered/compose.yaml", "services: {}")                    // already registered: skipped

	got := ScanOthers([]string{root}, expected, map[string]bool{filepath.Join(root, "registered"): true})
	names := []string{}
	for _, o := range got {
		names = append(names, o.Folder)
	}
	if len(got) != 2 || got[0].Folder != "deep" || got[1].Folder != "Other App" {
		t.Fatalf("want [deep, Other App], got %v", names)
	}
	deep, other := got[0], got[1]
	if len(deep.Composes) != 2 || deep.Composes[0] != "deployments/docker/docker-compose.prod.yml" {
		t.Errorf("compose files should be listed shallowest first then by name: %v", deep.Composes)
	}
	if deep.HasGit || deep.SuggestedName != "deep" {
		t.Errorf("deep: %+v", deep)
	}
	if !other.HasGit || other.Remote != "git@github.com:me/other.git" || other.Branch != "dev" || other.SuggestedName != "other-app" {
		t.Errorf("other: %+v", other)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Other App": "other-app", "SB-Web-APP": "sb-web-app", "couchdb_3.5_install": "couchdb-3-5-install", "--x--": "x"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q)=%q want %q", in, got, want)
		}
	}
}
