package security

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeDB serves a tiny copy of the vulnerability database and counts what was asked for.
func fakeDB(t *testing.T) (*VulnChecker, *int32, map[string]int) {
	var indexHits int32
	entryHits := map[string]int{}
	index := []map[string]any{
		{"path": "example.com/old", "vulns": []map[string]string{{"id": "GO-1", "fixed": "1.5.0"}}},
		{"path": "example.com/two/v2", "vulns": []map[string]string{{"id": "GO-2", "fixed": "1.2.0"}, {"id": "GO-2", "fixed": "2.5.0"}}},
		{"path": "example.com/unfixed", "vulns": []map[string]string{{"id": "GO-3"}}},
		{"path": "example.com/capped", "vulns": []map[string]string{{"id": "GO-4"}}},
	}
	entries := map[string]map[string]any{
		"GO-1": {"id": "GO-1", "summary": "Path traversal in old", "aliases": []string{"CVE-2020-1"},
			"affected":          []map[string]any{{"package": map[string]string{"name": "example.com/old"}, "ranges": []map[string]any{{"type": "SEMVER", "events": []map[string]string{{"introduced": "0"}, {"fixed": "1.5.0"}}}}}},
			"database_specific": map[string]string{"url": "https://pkg.go.dev/vuln/GO-1"}},
		"GO-2": {"id": "GO-2", "summary": "Two ranges",
			"affected": []map[string]any{{"package": map[string]string{"name": "example.com/two/v2"}, "ranges": []map[string]any{{"type": "SEMVER", "events": []map[string]string{{"introduced": "0"}, {"fixed": "1.2.0"}, {"introduced": "2.0.0"}, {"fixed": "2.5.0"}}}}}}},
		"GO-3": {"id": "GO-3", "summary": "Never fixed",
			"affected": []map[string]any{{"package": map[string]string{"name": "example.com/unfixed"}, "ranges": []map[string]any{{"type": "SEMVER", "events": []map[string]string{{"introduced": "1.0.0"}}}}}}},
		"GO-4": {"id": "GO-4", "summary": "Last affected",
			"affected": []map[string]any{{"package": map[string]string{"name": "example.com/capped"}, "ranges": []map[string]any{{"type": "SEMVER", "events": []map[string]string{{"introduced": "0"}, {"last_affected": "1.4.0"}}}}}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index/modules.json" {
			atomic.AddInt32(&indexHits, 1)
			json.NewEncoder(w).Encode(index)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/ID/"), ".json")
		entryHits[id]++
		if e, ok := entries[id]; ok {
			json.NewEncoder(w).Encode(e)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &VulnChecker{BaseURL: srv.URL}, &indexHits, entryHits
}

func TestCheckGoModFindsOnlyAffectedVersions(t *testing.T) {
	c, indexHits, entryHits := fakeDB(t)
	mod := `module app
go 1.25
require (
	example.com/old v1.2.0
	example.com/two/v2 v2.3.0
	example.com/unfixed v1.1.0 // indirect
	example.com/capped v1.5.0
	example.com/clean v1.0.0
	example.com/fine v1.9.9
)
`
	res, err := c.CheckGoMod(context.Background(), "go.mod", []byte(mod))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]VulnFinding{}
	for _, f := range res.Findings {
		got[f.Module] = f
	}
	if len(got) != 3 || res.Modules != 6 {
		t.Fatalf("expected old, two and unfixed to be flagged (capped is past its last affected version): %+v", res.Findings)
	}
	if v := got["example.com/old"].Vulns[0]; v.ID != "GO-1" || v.Fixed != "1.5.0" || v.Aliases[0] != "CVE-2020-1" || v.URL != "https://pkg.go.dev/vuln/GO-1" {
		t.Errorf("old: %+v", v)
	}
	if v := got["example.com/two/v2"].Vulns[0]; v.Fixed != "2.5.0" { // 2.3.0 is in the second range, not the first
		t.Errorf("two: the fix for the range the version is in, got %+v", v)
	}
	if v := got["example.com/unfixed"]; !v.Indirect || v.Vulns[0].Fixed != "" || v.Vulns[0].URL != "https://pkg.go.dev/vuln/GO-3" {
		t.Errorf("unfixed: %+v", v)
	}
	// a version past every fix is not looked up at all, and the index is downloaded once for any number of apps
	c.CheckGoMod(context.Background(), "go.mod", []byte("module b\nrequire example.com/old v1.9.0\n"))
	if entryHits["GO-1"] != 1 || atomic.LoadInt32(indexHits) != 1 {
		t.Errorf("old v1.9.0 is past the fix; the entry is cached: entry hits %v, index hits %d", entryHits, *indexHits)
	}
	if r, _ := c.CheckGoMod(context.Background(), "go.mod", []byte("module b\nrequire example.com/old v1.9.0\n")); len(r.Findings) != 0 {
		t.Errorf("a fixed version is clean: %+v", r.Findings)
	}
	if _, err := c.CheckGoMod(context.Background(), "go.mod", []byte("not a go.mod {{{")); err == nil {
		t.Error("a broken go.mod is an error, not an empty result")
	}
}

func TestDatabaseUnreachableIsAnError(t *testing.T) {
	c := &VulnChecker{BaseURL: "http://127.0.0.1:1"}
	if _, err := c.CheckGoMod(context.Background(), "go.mod", []byte("module b\nrequire example.com/old v1.0.0\n")); err == nil {
		t.Fatal("no answer from the database must not look like a clean result")
	}
}

func TestFindGoMods(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"go.mod", "go-api/go.mod", "tools/x/go.mod", "node_modules/pkg/go.mod", ".git/x/go.mod", "a/b/c/go.mod"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte("module m\n"), 0o644)
	}
	got := strings.Join(FindGoMods(dir, filepath.Glob), ",")
	if got != "go-api/go.mod,go.mod,tools/x/go.mod" {
		t.Fatalf("root, one and two levels down, no dependencies or hidden folders: %s", got)
	}
}
