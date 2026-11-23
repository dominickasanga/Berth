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
		Branch: "main", Subfolder: "go-api", ComposeFile: "docker-compose.yml", PredeployService: "migrate", FolderNames: []string{"kite360-api"}, Monitor: true},
		{Name: "other-app", Title: "Other", Repo: "https://github.com/dominickasanga/Other", Branch: "main", ComposeFile: "docker-compose.yml", FolderNames: []string{"other-app"}}}

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
	mk(varWww, "other-app", "https://github.com/someone/else.git") // name matches, remote does not

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

	reg := func(expected, mode string) string {
		form := url.Values{"expected": {expected}}
		if mode != "" {
			form.Set("mode", mode)
		}
		_, _, loc := do("POST", "/discover/register", form)
		return loc
	}
	// the user has to choose: no mode is refused
	if loc := reg("kite360-api", ""); !strings.Contains(loc, "err=") || !strings.Contains(loc, "Choose+how") {
		t.Fatalf("register without choosing a mode must be refused, got %q", loc)
	}
	// "existing folder" is blocked while two copies exist and none is flagged
	if loc := reg("kite360-api", "existing"); !strings.Contains(loc, "err=") || !strings.Contains(loc, "more+than+one+place") {
		t.Fatalf("existing without a flag must be refused, got %q", loc)
	}
	// flagging a path that was not found is refused
	if _, _, loc := do("POST", "/discover/choose", url.Values{"expected": {"kite360-api"}, "path": {"/etc"}}); !strings.Contains(loc, "err=") {
		t.Fatal("choosing a path outside the scan must be refused")
	}
	do("POST", "/discover/choose", url.Values{"expected": {"kite360-api"}, "path": {second}})
	_, page, _ = do("GET", "/discover", nil)
	if !strings.Contains(page, "Using this copy") || !strings.Contains(page, `value="existing"`) || !strings.Contains(page, `value="clone"`) {
		t.Fatal("flagged copy should be marked and both run modes offered")
	}

	// a folder whose name matches but whose git remote is a different repository cannot be used directly
	if loc := reg("other-app", "existing"); !strings.Contains(loc, "err=") || !strings.Contains(loc, "not+the+expected+repository") {
		t.Fatalf("existing with a wrong remote must be refused, got %q", loc)
	}
	// ...but a new clone is always possible
	if loc := reg("other-app", "clone"); !strings.HasPrefix(loc, "/apps/other-app") {
		t.Fatalf("clone should register, got %q", loc)
	}
	other, _ := srv.Store.AppByName(context.Background(), "other-app")
	if other == nil || other.SourceMode != "clone" || other.RepoURL != "https://github.com/dominickasanga/Other" || other.Branch != "main" {
		t.Fatalf("clone app should use the expected repo, got %+v", other)
	}

	// use the existing folder
	if loc := reg("kite360-api", "existing"); loc != "/apps/kite360-api?msg=registered+from+discovery" {
		t.Fatalf("register failed: %s", loc)
	}
	app, _ := srv.Store.AppByName(context.Background(), "kite360-api")
	if app == nil || app.SourceMode != "existing" || !app.Monitor || app.SourcePath != second || app.PredeploySvc != "migrate" || app.Subfolder != "go-api" {
		t.Fatalf("app not registered as expected: %+v", app)
	}
	if strings.Contains(app.RepoURL, "secret") || strings.Contains(app.RepoURL, "tok") {
		t.Fatalf("credentials were stored in the database: %s", app.RepoURL)
	}
	if app.Branch != "release" {
		t.Fatalf("branch should come from the existing folder, got %s", app.Branch)
	}
	_, appPage, _ := do("GET", "/apps/kite360-api", nil)
	if !strings.Contains(appPage, "Existing folder") || !strings.Contains(appPage, "Change how it runs") {
		t.Fatal("app page should show the mode badge and the control to change it")
	}
	_, listPage, _ := do("GET", "/", nil)
	if !strings.Contains(listPage, "Existing folder") || !strings.Contains(listPage, "Berth clone") {
		t.Fatal("the Apps list should show how each app runs")
	}

	// switching an already-registered app
	src := func(name, mode string) string {
		form := url.Values{}
		if mode != "" {
			form.Set("mode", mode)
		}
		_, _, loc := do("POST", "/apps/"+name+"/source", form)
		return loc
	}
	if loc := src("kite360-api", ""); !strings.Contains(loc, "err=") {
		t.Fatalf("no choice must be refused, got %q", loc)
	}
	if loc := src("kite360-api", "existing"); !strings.Contains(loc, "Already") {
		t.Fatalf("same mode should be a no-op, got %q", loc)
	}
	if loc := src("kite360-api", "clone"); !strings.Contains(loc, "msg=") {
		t.Fatalf("switch to clone failed: %q", loc)
	}
	app, _ = srv.Store.AppByName(context.Background(), "kite360-api")
	if app.SourceMode != "clone" || app.SourcePath != second || strings.Contains(app.RepoURL, "secret") {
		t.Fatalf("after switching to clone: %+v", app)
	}
	if loc := src("kite360-api", "existing"); !strings.Contains(loc, "msg=") {
		t.Fatalf("switch back to the existing folder failed: %q", loc)
	}
	app, _ = srv.Store.AppByName(context.Background(), "kite360-api")
	if app.SourceMode != "existing" || app.SourcePath != second || app.Branch != "release" {
		t.Fatalf("after switching back: %+v", app)
	}
	// a folder with the wrong remote, and an app added by hand, cannot use "existing"
	if loc := src("other-app", "existing"); !strings.Contains(loc, "err=") || !strings.Contains(loc, "not+the+expected+repository") {
		t.Fatalf("wrong remote must be refused, got %q", loc)
	}
	do("POST", "/apps", url.Values{"name": {"manual"}, "repo_url": {"https://example.com/m.git"}, "branch": {"main"}, "compose_file": {"docker-compose.yml"}})
	if loc := src("manual", "existing"); !strings.Contains(loc, "err=") || !strings.Contains(loc, "added+by+hand") {
		t.Fatalf("a hand-added app has no known folder, got %q", loc)
	}
	// a running deploy blocks switching
	held := make(chan struct{})
	go srv.Deploy.Idle(app.ID, func() error { <-held; return nil })
	time.Sleep(100 * time.Millisecond)
	if loc := src("kite360-api", "clone"); !strings.Contains(loc, "deploy+is+running") {
		t.Fatalf("switching during a deploy must be refused, got %q", loc)
	}
	close(held)
}
