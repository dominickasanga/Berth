package receiver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dominickasanga/berth/internal/receiver/retain"
)

type rig struct {
	t   *testing.T
	cfg *config
	ts  *httptest.Server
}

func newRig(t *testing.T, mutate func(*config)) *rig {
	t.Helper()
	root := t.TempDir()
	cfg := &config{
		basePath:    "/beam/",
		uploadDir:   filepath.Join(root, "uploads"),
		receivedDir: filepath.Join(root, "received"),
		clientPath:  filepath.Join(root, "tusk-send"),
		agentPath:   filepath.Join(root, "tusk-agent"),
		username:    "admin",
		password:    "upload-secret",
	}
	for _, d := range []string{cfg.uploadDir, cfg.receivedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if mutate != nil {
		mutate(cfg)
	}
	h, err := cfg.handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &rig{t, cfg, ts}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func meta(kv map[string]string) string {
	var parts []string
	for k, v := range kv {
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}

func (r *rig) do(method, url string, hdr map[string]string, body io.Reader, auth bool) *http.Response {
	r.t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if auth {
		req.SetBasicAuth("admin", "upload-secret")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// upload performs a tus create followed by one PATCH with the whole body, as the agent does for a small
// file. It returns the status of the create and of the final PATCH (0 when the create was refused).
func (r *rig) upload(md map[string]string, body []byte) (create, patch int) {
	r.t.Helper()
	resp := r.do("POST", r.ts.URL+"/beam/", map[string]string{
		"Upload-Length": strconv.Itoa(len(body)), "Upload-Metadata": meta(md),
	}, nil, true)
	if resp.StatusCode != http.StatusCreated {
		return resp.StatusCode, 0
	}
	loc := resp.Header.Get("Location")
	p := r.do("PATCH", loc, map[string]string{"Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"}, strings.NewReader(string(body)), true)
	return resp.StatusCode, p.StatusCode
}

func (r *rig) received(rel string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(r.cfg.receivedDir, rel))
	return b, err == nil
}

func (r *rig) scratchLeftovers() int {
	entries, _ := os.ReadDir(r.cfg.uploadDir)
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

func TestEveryRouteNeedsTheUploadPassword(t *testing.T) {
	r := newRig(t, nil)
	for _, path := range []string{"/beam/", "/beam/install.sh", "/beam/get-agent", "/beam/get-client"} {
		resp := r.do("POST", r.ts.URL+path, map[string]string{"Upload-Length": "1"}, nil, false)
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != `Basic realm="TusK"` {
			t.Errorf("%s without credentials: %d %q", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	req, _ := http.NewRequest("POST", r.ts.URL+"/beam/", nil)
	req.SetBasicAuth("admin", "wrong")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Errorf("a wrong password must be refused, got %d", resp.StatusCode)
	}
	// the old dashboard is gone: nothing answers at /beam/admin/ with the admin login any more
	if resp := r.do("GET", r.ts.URL+"/beam/admin/", nil, nil, true); resp.StatusCode < 400 {
		t.Errorf("the old dashboard path should not serve a page, got %d", resp.StatusCode)
	}
}

func TestVerifiedUploadIsFiledByItsRelpath(t *testing.T) {
	r := newRig(t, nil)
	body := []byte(strings.Repeat("backup-bytes-", 500))
	c, p := r.upload(map[string]string{"relpath": "documents/2026/incr-1.tar.gz", "sha256": sum(body)}, body)
	if c != 201 || p != 204 {
		t.Fatalf("create %d patch %d", c, p)
	}
	got, ok := r.received("documents/2026/incr-1.tar.gz")
	if !ok || string(got) != string(body) {
		t.Fatal("the file should land under its relpath, byte for byte")
	}
	if n := r.scratchLeftovers(); n != 0 {
		t.Errorf("the scratch directory should be empty after a placed upload, found %d entries", n)
	}
}

func TestBadChecksumIsRefusedAndNotStored(t *testing.T) {
	r := newRig(t, nil)
	body := []byte("the real content")
	_, p := r.upload(map[string]string{"relpath": "backups/x.dump", "sha256": sum([]byte("something else"))}, body)
	if p != 500 { // a 5xx on purpose: the sender retries, corruption is usually transient
		t.Fatalf("a checksum mismatch should be a retryable 5xx, got %d", p)
	}
	if _, ok := r.received("backups/x.dump"); ok {
		t.Fatal("a corrupt upload must never reach the received tree")
	}
	if n := r.scratchLeftovers(); n != 0 {
		t.Errorf("a rejected upload must not leave its payload behind, found %d entries", n)
	}
}

func TestMissingChecksumIsRefusedUnlessAllowed(t *testing.T) {
	body := []byte("no checksum")
	r := newRig(t, nil)
	if _, p := r.upload(map[string]string{"relpath": "backups/y.dump"}, body); p != 400 {
		t.Fatalf("no sha256 should be a 400, got %d", p)
	}
	if _, ok := r.received("backups/y.dump"); ok {
		t.Fatal("an unverifiable upload must not be stored")
	}
	r = newRig(t, func(c *config) { c.allowUnverified = true })
	if _, p := r.upload(map[string]string{"relpath": "backups/y.dump"}, body); p != 204 {
		t.Fatalf("TUSK_ALLOW_UNVERIFIED should accept it, got %d", p)
	}
}

func TestPathEscapesAreRefusedWithA4xx(t *testing.T) {
	r := newRig(t, nil)
	body := []byte("x")
	for _, rel := range []string{"../escaped", "a/../../escaped", "/etc/cron.d/backdoor"} {
		_, p := r.upload(map[string]string{"relpath": rel, "sha256": sum(body)}, body)
		if p != 400 { // 4xx so the client does not retry a request that can never succeed
			t.Errorf("%q should be a 400, got %d", rel, p)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(r.cfg.receivedDir), "escaped")); err == nil {
		t.Fatal("nothing may be written outside the received directory")
	}
	if n := r.scratchLeftovers(); n != 0 {
		t.Errorf("rejected uploads must not stay in the scratch directory, found %d", n)
	}
}

func TestUploadThatWouldBreachTheReserveGets507BeforeAnyData(t *testing.T) {
	r := newRig(t, func(c *config) { c.minFreeBytes = 1 << 62 })
	c, _ := r.upload(map[string]string{"relpath": "documents/big.tar.gz", "sha256": sum([]byte("x"))}, []byte("x"))
	if c != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 at creation, got %d", c)
	}
}

func TestRunReportsAreStoredAndNeverPruned(t *testing.T) {
	policy := &retain.Policy{Daily: 1}
	r := newRig(t, func(c *config) {
		c.retention, c.retentionDirs = policy, map[string]bool{"reports": true, "backups": true}
	})
	// even if "reports" were nominated for retention, a report is data about a backup, not a backup
	os.MkdirAll(filepath.Join(r.cfg.receivedDir, "reports"), 0o755)
	old := filepath.Join(r.cfg.receivedDir, "reports", "tams-2020-01-01_000000.json")
	os.WriteFile(old, []byte("{}"), 0o644)
	body := []byte(`{"host":"tams"}`)
	if _, p := r.upload(map[string]string{"relpath": "reports/2026-11-27_21-00-00_UTC.json", "sha256": sum(body)}, body); p != 204 {
		t.Fatalf("report upload: %d", p)
	}
	if _, ok := r.received("reports/2026-11-27_21-00-00_UTC.json"); !ok {
		t.Fatal("the report should be stored as it arrived")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("reports must never be pruned")
	}
}

func seedDumps(t *testing.T, dir string, days ...string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	for _, d := range days {
		os.WriteFile(filepath.Join(dir, d+"_21-00-00_UTC.dump.tar.gz"), []byte("old"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "notes-from-a-human.txt"), []byte("keep me"), 0o644) // no timestamp
}

func TestRetentionPrunesOnlyNominatedDirsAndNeverWhatItCannotParse(t *testing.T) {
	r := newRig(t, func(c *config) {
		c.retention, c.retentionDirs = &retain.Policy{Daily: 2}, map[string]bool{"backups": true}
	})
	seedDumps(t, filepath.Join(r.cfg.receivedDir, "backups"), "2026-11-20", "2026-11-21", "2026-11-22", "2026-11-23")
	seedDumps(t, filepath.Join(r.cfg.receivedDir, "documents"), "2026-11-20", "2026-11-21", "2026-11-22")
	body := []byte("tonight's dump")
	if _, p := r.upload(map[string]string{"relpath": "backups/2026-11-24_21-00-00_UTC.dump.tar.gz", "sha256": sum(body)}, body); p != 204 {
		t.Fatalf("upload: %d", p)
	}
	left := func(dir string) []string {
		es, _ := os.ReadDir(filepath.Join(r.cfg.receivedDir, dir))
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}
	got := strings.Join(left("backups"), " ")
	if !strings.Contains(got, "2026-11-24") || !strings.Contains(got, "2026-11-23") || strings.Contains(got, "2026-11-22") || strings.Contains(got, "2026-11-20") {
		t.Errorf("the newest two days should remain in backups: %s", got)
	}
	if !strings.Contains(got, "notes-from-a-human.txt") {
		t.Errorf("a file with no timestamp is never deleted: %s", got)
	}
	if len(left("documents")) != 4 {
		t.Errorf("a directory that was not nominated is never touched: %v", left("documents"))
	}
}

func TestRetentionDryRunDeletesNothing(t *testing.T) {
	r := newRig(t, func(c *config) {
		c.retention, c.retentionDirs, c.retentionDryRun = &retain.Policy{Daily: 1}, map[string]bool{"backups": true}, true
	})
	seedDumps(t, filepath.Join(r.cfg.receivedDir, "backups"), "2026-11-20", "2026-11-21", "2026-11-22")
	body := []byte("tonight")
	r.upload(map[string]string{"relpath": "backups/2026-11-23_21-00-00_UTC.dump.tar.gz", "sha256": sum(body)}, body)
	es, _ := os.ReadDir(filepath.Join(r.cfg.receivedDir, "backups"))
	if len(es) != 5 { // 3 seeded + notes + the new one
		t.Fatalf("a dry run must delete nothing, %d files remain", len(es))
	}
}

func TestSenderBootstrapRoutes(t *testing.T) {
	r := newRig(t, nil)
	if resp := r.do("GET", r.ts.URL+"/beam/get-agent", nil, nil, true); resp.StatusCode != 404 {
		t.Fatalf("a missing agent binary is a 404, got %d", resp.StatusCode)
	}
	os.WriteFile(r.cfg.agentPath, []byte("AGENT-BINARY"), 0o755)
	os.WriteFile(r.cfg.clientPath, []byte("CLIENT-BINARY"), 0o755)
	for path, want := range map[string]string{"/beam/get-agent": "AGENT-BINARY", "/beam/get-client": "CLIENT-BINARY"} {
		resp := r.do("GET", r.ts.URL+path, nil, nil, true)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(b) != want {
			t.Errorf("%s: %d %q", path, resp.StatusCode, b)
		}
	}
	// the installer points back at the URL the client used, through a TLS-terminating proxy
	resp := r.do("GET", r.ts.URL+"/beam/install.sh", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "proworkwave.com"}, nil, true)
	b, _ := io.ReadAll(resp.Body)
	script := string(b)
	if resp.StatusCode != 200 || !strings.HasPrefix(script, "#!/bin/sh") || !strings.Contains(script, `SERVER_URL="https://proworkwave.com/beam/"`) || strings.Contains(script, "__SERVER_URL__") {
		t.Errorf("installer: %d, url not filled in: %.200s", resp.StatusCode, script)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/x-shellscript") {
		t.Errorf("installer content type: %q", resp.Header.Get("Content-Type"))
	}
}

func TestConfigFromEnvironment(t *testing.T) {
	clear := func() {
		for _, k := range []string{"TUSK_PASSWORD", "TUSK_PORT", "TUSK_BASE_PATH", "TUSK_RETENTION", "TUSK_RETENTION_DIRS", "TUSK_RETENTION_DRYRUN", "TUSK_MIN_FREE_BYTES", "TUSK_ALLOW_UNVERIFIED"} {
			t.Setenv(k, "")
		}
	}
	clear()
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "TUSK_PASSWORD") {
		t.Fatalf("no password, no start: %v", err)
	}
	t.Setenv("TUSK_PASSWORD", "x")
	c, err := loadConfig()
	if err != nil || c.port != "5183" || c.basePath != "/beam/" || c.username != "admin" || c.minFreeBytes != 1<<30 || c.retention != nil || c.allowUnverified {
		t.Fatalf("defaults must match TusK's: %+v %v", c, err)
	}
	t.Setenv("TUSK_BASE_PATH", "/beam")
	t.Setenv("TUSK_RETENTION", "7,4,12")
	t.Setenv("TUSK_RETENTION_DRYRUN", "1")
	t.Setenv("TUSK_MIN_FREE_BYTES", "10737418240")
	c, err = loadConfig()
	if err != nil || c.basePath != "/beam/" || c.retention == nil || *c.retention != (retain.Policy{Daily: 7, Weekly: 4, Monthly: 12}) || !c.retentionDryRun || !c.retentionDirs["backups"] || c.minFreeBytes != 10<<30 {
		t.Fatalf("the values from the live receiver's environment: %+v %v", c, err)
	}
	for _, bad := range []string{"7,4", "a,b,c", "0,0,0", "-1,2,3"} {
		t.Setenv("TUSK_RETENTION", bad)
		if _, err := loadConfig(); err == nil {
			t.Errorf("retention %q should be refused", bad)
		}
	}
}
