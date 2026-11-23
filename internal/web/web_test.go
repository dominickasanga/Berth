package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dominickasanga/berth/config"
	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
	"github.com/dominickasanga/berth/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

func setup(t *testing.T) (*httptest.Server, string) {
	ts, secret, _ := setupServer(t)
	return ts, secret
}

func setupServer(t *testing.T) (*httptest.Server, string, *Server) {
	dsn := os.Getenv("BERTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BERTH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	// Start from an empty schema so the real migrations (and their seeded defaults) run.
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := crypt.New(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	key, _ := totp.Generate(totp.GenerateOpts{Issuer: "Berth", AccountName: "admin"})
	sealed, _ := box.Seal(key.Secret())
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct horse battery"), 4)
	if err := st.CreateAdmin(ctx, "admin", string(hash), sealed); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: st, Box: box, Deploy: &deploy.Engine{Store: st, Box: box, DataDir: t.TempDir()}}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, key.Secret(), srv
}

func setupLoggedIn(t *testing.T) (*httptest.Server, string, *Server, *http.Cookie) {
	ts, secret, srv := setupServer(t)
	code, _ := totp.GenerateCode(secret, time.Now())
	resp, _ := client().PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"correct horse battery"}, "code": {code}})
	return ts, secret, srv, resp.Cookies()[0]
}

func client() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestLoginAndSecrets(t *testing.T) {
	ts, secret := setup(t)
	c := client()

	// protected pages redirect to login
	resp, _ := c.Get(ts.URL + "/")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// wrong 2FA code is refused even with the right password
	resp, _ = c.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"correct horse battery"}, "code": {"000000"}})
	if resp.StatusCode != 401 {
		t.Fatalf("bad code should be 401, got %d", resp.StatusCode)
	}

	// correct password + code logs in
	code, _ := totp.GenerateCode(secret, time.Now())
	resp, _ = c.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"correct horse battery"}, "code": {code}})
	if resp.StatusCode != 303 || len(resp.Cookies()) == 0 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	cookie := resp.Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie must be HttpOnly + SameSite=Strict")
	}
	do := func(method, path string, form url.Values) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}

	// register an app, add a secret and a plain var
	if code, _ := do("POST", "/apps", url.Values{"name": {"demo"}, "repo_url": {"https://example.com/r.git"}, "branch": {"main"}, "compose_file": {"docker-compose.yml"}}); code != 303 {
		t.Fatalf("create app: %d", code)
	}
	do("POST", "/apps/demo/env", url.Values{"key": {"DB_PASSWORD"}, "value": {"s3cr3t-value"}, "secret": {"on"}})
	do("POST", "/apps/demo/env", url.Values{"key": {"LOG_LEVEL"}, "value": {"debug"}})
	_, page := do("GET", "/apps/demo", nil)
	if strings.Contains(page, "s3cr3t-value") {
		t.Fatal("secret value leaked into the page")
	}
	if !strings.Contains(page, "DB_PASSWORD") || !strings.Contains(page, "debug") {
		t.Fatal("expected key names and non-secret value on the page")
	}

	// cross-origin POST is refused
	req, _ := http.NewRequest("POST", ts.URL+"/apps/demo/deploy", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(cookie)
	r, _ := c.Do(req)
	if r.StatusCode != 403 {
		t.Fatalf("cross-origin POST should be 403, got %d", r.StatusCode)
	}

	// invalid app input is rejected
	if code, _ := do("POST", "/apps", url.Values{"name": {"bad"}, "repo_url": {"--upload-pack=x"}, "branch": {"main"}, "compose_file": {"docker-compose.yml"}}); code != 400 {
		t.Fatalf("option-like repo URL should be 400, got %d", code)
	}
}

func TestDiscoverTwoCopiesNeedAFlag(t *testing.T) {
	ts, secret, srv, cookie := setupLoggedIn(t)
	_ = secret
	srv.Expected = []config.ExpectedApp{{Name: "kite360-api", Title: "Kite360 API", Repo: "https://github.com/dominickasanga/Kite360-API",
		Branch: "main", Subfolder: "go-api", ComposeFile: "docker-compose.yml", PredeployService: "migrate", FolderNames: []string{"kite360-api"}, Monitor: true}}

	mk := func(parent, name, remote string) string {
		dir := filepath.Join(parent, name)
		os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
		os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/release\n"), 0o644)
		return dir
	}
	varWww, blockvol := t.TempDir(), t.TempDir()
	mk(varWww, "Kite360-API", "git@github.com:dominickasanga/Kite360-API.git")
	second := mk(blockvol, "kite360-api", "https://tok:secret@github.com/dominickasanga/Kite360-API.git")

	c := client()
	do := func(method, path string, form url.Values) (int, string, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b), r.Header.Get("Location")
	}

	// the migration seeds the two default scan paths; swap them for temp dirs
	_, page, _ := do("GET", "/discover", nil)
	if !strings.Contains(page, "/var/www") || !strings.Contains(page, "/mnt/blockvolume") {
		t.Fatal("default scan paths /var/www and /mnt/blockvolume should be listed")
	}
	do("POST", "/discover/paths/delete", url.Values{"path": {"/var/www"}})
	do("POST", "/discover/paths/delete", url.Values{"path": {"/mnt/blockvolume"}})
	if code, _, loc := do("POST", "/discover/paths", url.Values{"path": {"relative/path"}}); code != 303 || !strings.Contains(loc, "err=") {
		t.Fatalf("relative path must be refused, got %d %s", code, loc)
	}
	do("POST", "/discover/paths", url.Values{"path": {varWww}})
	do("POST", "/discover/paths", url.Values{"path": {blockvol}})

	_, page, _ = do("GET", "/discover", nil)
	if !strings.Contains(page, "Found in 2 places") || strings.Count(page, "Use this copy") != 2 {
		t.Fatalf("two copies should ask for a flag:\n%s", page)
	}
	if strings.Contains(page, "secret@") || strings.Contains(page, "ghp_") || strings.Contains(page, "tok:") {
		t.Fatal("credentials from a git remote leaked into the page")
	}

	// registering is blocked until a copy is flagged
	if _, _, loc := do("POST", "/discover/register", url.Values{"expected": {"kite360-api"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("register without a flag must be refused, got redirect %q", loc)
	}
	// flagging a path that was not found is refused
	if _, _, loc := do("POST", "/discover/choose", url.Values{"expected": {"kite360-api"}, "path": {"/etc"}}); !strings.Contains(loc, "err=") {
		t.Fatal("choosing a path outside the scan must be refused")
	}
	do("POST", "/discover/choose", url.Values{"expected": {"kite360-api"}, "path": {second}})
	_, page, _ = do("GET", "/discover", nil)
	if !strings.Contains(page, "Using this copy") {
		t.Fatal("flagged copy should be marked")
	}

	code, _, loc := do("POST", "/discover/register", url.Values{"expected": {"kite360-api"}})
	if code != 303 || loc != "/apps/kite360-api?msg=registered+from+discovery" {
		t.Fatalf("register failed: %d %s", code, loc)
	}
	app, _ := srv.Store.AppByName(context.Background(), "kite360-api")
	if app == nil || !app.Monitor || app.SourcePath != second || app.PredeploySvc != "migrate" || app.Subfolder != "go-api" {
		t.Fatalf("app not registered as expected: %+v", app)
	}
	if strings.Contains(app.RepoURL, "secret") || strings.Contains(app.RepoURL, "tok") {
		t.Fatalf("credentials were stored in the database: %s", app.RepoURL)
	}
	if app.Branch != "release" {
		t.Fatalf("branch should come from the flagged copy, got %s", app.Branch)
	}
}
