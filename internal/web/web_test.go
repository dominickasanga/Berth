package web

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dominickasanga/berth/config"
	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
	"github.com/dominickasanga/berth/internal/logs"
	"github.com/dominickasanga/berth/internal/monitor"
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
	return setupServerBase(t, "")
}

func setupServerBase(t *testing.T, base string) (*httptest.Server, string, *Server) {
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
	srv := &Server{Store: st, Box: box, BasePath: base, Deploy: &deploy.Engine{Store: st, Box: box, DataDir: t.TempDir()}}
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

func TestServedUnderBasePath(t *testing.T) {
	ts, secret, _ := setupServerBase(t, "/_berth")
	c := client()
	get := func(path string, cookie *http.Cookie) (int, string, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b), r.Header.Get("Location")
	}

	// the site root and the bare prefix lead to Berth; other paths outside the prefix do not exist
	if code, _, loc := get("/", nil); code != 302 || loc != "/_berth/" {
		t.Fatalf("/ should redirect to /_berth/, got %d %q", code, loc)
	}
	if code, _, loc := get("/_berth", nil); code != 302 || loc != "/_berth/" {
		t.Fatalf("/_berth should redirect to /_berth/, got %d %q", code, loc)
	}
	if code, _, _ := get("/login", nil); code != 404 {
		t.Fatalf("/login outside the prefix must be 404, got %d", code)
	}
	// protected pages send you to the prefixed login
	if code, _, loc := get("/_berth/", nil); code != 303 || loc != "/_berth/login" {
		t.Fatalf("expected redirect to /_berth/login, got %d %q", code, loc)
	}
	// static files are served under the prefix, and the login page links to them there
	if code, _, _ := get("/_berth/static/app.css", nil); code != 200 {
		t.Fatalf("static file under the prefix: %d", code)
	}
	code, page, _ := get("/_berth/login", nil)
	if code != 200 || !strings.Contains(page, `action="/_berth/login"`) {
		t.Fatalf("login page should post to the prefixed URL (%d)", code)
	}

	// log in
	totpCode, _ := totp.GenerateCode(secret, time.Now())
	resp, _ := c.PostForm(ts.URL+"/_berth/login", url.Values{"username": {"admin"}, "password": {"correct horse battery"}, "code": {totpCode}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/_berth/" {
		t.Fatalf("login should redirect to /_berth/, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookie := resp.Cookies()[0]
	if cookie.Path != "/_berth" {
		t.Fatalf("session cookie should be limited to the prefix, got %q", cookie.Path)
	}

	// create an app, then check that no page links outside the prefix
	post := func(path string, form url.Values) (int, string) {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		r, _ := c.Do(req)
		return r.StatusCode, r.Header.Get("Location")
	}
	if code, loc := post("/_berth/apps", url.Values{"name": {"demo"}, "repo_url": {"https://example.com/r.git"}, "branch": {"main"}, "compose_file": {"docker-compose.yml"}}); code != 303 || loc != "/_berth/apps/demo" {
		t.Fatalf("create app redirect: %d %q", code, loc)
	}
	bare := regexp.MustCompile(`(?:href|src|action|hx-get|data-url)="/(?:[^_]|_[^b])`)
	for _, path := range []string{"/_berth/", "/_berth/apps/new", "/_berth/apps/demo", "/_berth/discover", "/_berth/apps/demo/status", "/_berth/apps/demo/status?view=compact"} {
		code, body, _ := get(path, cookie)
		if code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
		if m := bare.FindString(body); m != "" {
			t.Errorf("%s has a link outside the base path: %s", path, m)
		}
		if strings.Contains(body, `href="/static/`) || strings.Contains(body, `src="/static/`) {
			t.Errorf("%s links to /static outside the base path", path)
		}
	}
	_, appPage, _ := get("/_berth/apps/demo", cookie)
	for _, want := range []string{`data-base="/_berth"`, `hx-get="/_berth/apps/demo/status"`, `data-url="/_berth/apps/demo/logs/stream"`, `href="/_berth/static/icons.svg`} {
		if !strings.Contains(appPage, want) && !strings.Contains(appPage, strings.Replace(want, `href="/_berth/static/icons.svg`, `href="/_berth/static/icons.svg`, 1)) {
			t.Errorf("app page missing %s", want)
		}
	}
	// the logged-out redirect for an htmx request also respects the prefix
	req, _ := http.NewRequest("GET", ts.URL+"/_berth/apps/demo/status", nil)
	req.Header.Set("HX-Request", "true")
	r, _ := c.Do(req)
	if r.Header.Get("HX-Redirect") != "/_berth/login" {
		t.Fatalf("htmx redirect should be /_berth/login, got %q", r.Header.Get("HX-Redirect"))
	}
}

func TestNormalizeBase(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "/_berth": "/_berth", "_berth/": "/_berth", " /ops/berth ": "/ops/berth"} {
		if got, err := NormalizeBase(in); err != nil || got != want {
			t.Errorf("NormalizeBase(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/a b", "/../x", "/x?y", "//host", `/x\`} {
		if _, err := NormalizeBase(bad); err == nil {
			t.Errorf("NormalizeBase(%q) should be rejected", bad)
		}
	}
}

func TestDiscoverOtherAppsCanBeAdded(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	srv.Expected = []config.ExpectedApp{{Name: "kite360-api", Title: "Kite360 API", Repo: "https://github.com/dominickasanga/Kite360-API", FolderNames: []string{"kite360-api"}}}

	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	mk("Cool Thing/docker-compose.yml", "services: {}")
	mk("Cool Thing/.git/config", "[remote \"origin\"]\n\turl = https://tok:secret@github.com/me/cool.git\n")
	mk("Cool Thing/.git/HEAD", "ref: refs/heads/dev\n")
	mk("nogit/deployments/docker/compose.prod.yaml", "services: {}") // compose file but no git remote
	mk("kite360-api/go-api/docker-compose.yml", "services: {}")      // expected app: never listed as "other"
	cool := filepath.Join(root, "Cool Thing")

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
	do("POST", "/discover/paths/delete", url.Values{"path": {"/var/www"}})
	do("POST", "/discover/paths/delete", url.Values{"path": {"/mnt/blockvolume"}})
	do("POST", "/discover/paths", url.Values{"path": {root}})

	_, page, _ := do("GET", "/discover", nil)
	if !strings.Contains(page, "Other apps found") || !strings.Contains(page, "Cool Thing") || !strings.Contains(page, "nogit") {
		t.Fatal("the folders with a compose file should be listed")
	}
	if strings.Contains(page, `value="`+filepath.Join(root, "kite360-api")+`"`) {
		t.Fatal("an expected app must not be listed as an other app")
	}
	if strings.Contains(page, "secret") || strings.Contains(page, "tok:") {
		t.Fatal("credentials from a git remote leaked into the page")
	}

	add := func(form url.Values) string {
		_, _, loc := do("POST", "/discover/add", form)
		return loc
	}
	base := func() url.Values {
		return url.Values{"path": {cool}, "name": {"cool-thing"}, "compose": {"docker-compose.yml"}, "mode": {"existing"}}
	}
	// the form cannot point Berth at a path the scan did not find, or at an unlisted compose file
	f := base()
	f.Set("path", "/etc")
	if loc := add(f); !strings.Contains(loc, "err=") {
		t.Fatalf("a path outside the scan must be refused, got %q", loc)
	}
	f = base()
	f.Set("compose", "../../etc/passwd")
	if loc := add(f); !strings.Contains(loc, "err=") {
		t.Fatalf("an unlisted compose file must be refused, got %q", loc)
	}
	f = base()
	f.Set("name", "Bad Name")
	if loc := add(f); !strings.Contains(loc, "err=") {
		t.Fatalf("a bad name must be refused, got %q", loc)
	}
	f = base()
	f.Del("mode")
	if loc := add(f); !strings.Contains(loc, "Choose+how") {
		t.Fatalf("no mode must be refused, got %q", loc)
	}
	// a folder with no git remote cannot be added
	if loc := add(url.Values{"path": {filepath.Join(root, "nogit")}, "name": {"nogit"}, "compose": {"deployments/docker/compose.prod.yaml"}, "mode": {"existing"}}); !strings.Contains(loc, "no+git+remote") {
		t.Fatalf("no remote must be refused, got %q", loc)
	}
	// a bad project name is refused
	f = base()
	f.Set("project", "Bad Project")
	if loc := add(f); !strings.Contains(loc, "err=") {
		t.Fatalf("a bad project name must be refused, got %q", loc)
	}

	// add it, adopting an existing stack
	f = base()
	f.Set("project", "cool")
	if loc := add(f); loc != "/apps/cool-thing?msg=added+from+discovery" {
		t.Fatalf("add failed: %q", loc)
	}
	app, _ := srv.Store.AppByName(context.Background(), "cool-thing")
	if app == nil || app.SourceMode != "existing" || app.SourcePath != cool || app.Branch != "dev" ||
		app.ComposeFile != "docker-compose.yml" || app.Subfolder != "" || app.ComposeProject != "cool" || app.Monitor {
		t.Fatalf("app not added as expected: %+v", app)
	}
	if strings.Contains(app.RepoURL, "secret") || strings.Contains(app.RepoURL, "tok") {
		t.Fatalf("credentials were stored: %s", app.RepoURL)
	}
	_, appPage, _ := do("GET", "/apps/cool-thing", nil)
	if !strings.Contains(appPage, "adopts the existing stack") {
		t.Fatal("the app page should show the adopted compose project")
	}
	// it is no longer listed as an other app
	_, page, _ = do("GET", "/discover", nil)
	if strings.Contains(page, `value="`+cool+`"`) {
		t.Fatal("an added folder must leave the other-apps list")
	}
}

func TestLogsPageAuditPageAndNoSecretsInLogs(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	hub := logs.NewHub(200)
	srv.Logs = hub
	old := log.Writer()
	log.SetOutput(hub) // what main does, so audit and error lines reach the Logs page
	defer log.SetOutput(old)

	c := client()
	get := func(path string, withCookie bool) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if withCookie {
			req.AddCookie(cookie)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	post := func(path string, form url.Values) {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		c.Do(req)
	}

	// both pages need a login
	for _, p := range []string{"/logs", "/logs/stream", "/audit"} {
		if code, _ := get(p, false); code != 303 {
			t.Errorf("%s without a login should redirect, got %d", p, code)
		}
	}
	// the pages render and are in the navigation
	_, page := get("/logs", true)
	if !strings.Contains(page, `data-url="/logs/stream"`) || !strings.Contains(page, `href="/audit"`) || !strings.Contains(page, `href="/logs"`) {
		t.Fatal("Logs page or navigation links missing")
	}

	// actions are audited and logged, but a secret value is in neither
	post("/apps", url.Values{"name": {"demo"}, "repo_url": {"https://example.com/r.git"}, "branch": {"main"}, "compose_file": {"docker-compose.yml"}})
	post("/apps/demo/env", url.Values{"key": {"DB_PASSWORD"}, "value": {"s3cr3t-value"}, "secret": {"on"}})
	_, audit := get("/audit", true)
	if !strings.Contains(audit, "app.create") || !strings.Contains(audit, "env.set") || !strings.Contains(audit, "demo:DB_PASSWORD") {
		t.Fatalf("audit page should list the actions:\n%s", audit)
	}
	if strings.Contains(audit, "s3cr3t-value") {
		t.Fatal("secret value leaked into the audit page")
	}
	recent, _, cancel := hub.Subscribe(200)
	cancel()
	if joined := strings.Join(recent, "\n"); !strings.Contains(joined, "audit: admin env.set demo:DB_PASSWORD") || strings.Contains(joined, "s3cr3t-value") {
		t.Fatalf("the log should record the action but never the value:\n%s", joined)
	}

	// the stream sends recent lines first, then live ones
	req, _ := http.NewRequest("GET", ts.URL+"/logs/stream", nil)
	req.AddCookie(cookie)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 50)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				lines <- strings.TrimPrefix(l, "data: ")
			}
		}
	}()
	waitFor := func(substr string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case l := <-lines:
				if strings.Contains(l, substr) {
					return
				}
			case <-deadline:
				t.Fatalf("did not see %q in the stream", substr)
			}
		}
	}
	waitFor("audit: admin env.set demo:DB_PASSWORD") // history
	log.Printf("a brand new live line")
	waitFor("a brand new live line") // live

	// paging: more than one page of audit rows
	for i := 0; i < 120; i++ {
		srv.Store.Audit(context.Background(), "admin", "test.event", fmt.Sprintf("n%03d", i))
	}
	_, p1 := get("/audit", true)
	m := regexp.MustCompile(`/audit\?before=(\d+)`).FindStringSubmatch(p1)
	if m == nil || strings.Count(p1, "<code>") != 100 {
		t.Fatalf("first page should have 100 rows and an Older link (rows=%d)", strings.Count(p1, "<code>"))
	}
	_, p2 := get("/audit?before="+m[1], true)
	if strings.Contains(p2, "n119") || !strings.Contains(p2, "app.create") {
		t.Fatal("the older page should continue where the first stopped")
	}
}

func TestServicesFlow(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	srv.Sampler = &monitor.Sampler{Store: srv.Store, Box: srv.Box}
	c := client()
	do := func(method, path string, form url.Values, withCookie bool) (int, string, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withCookie {
			req.AddCookie(cookie)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b), r.Header.Get("Location")
	}

	for _, p := range []string{"/services", "/services/new", "/services/1"} {
		if code, _, _ := do("GET", p, nil, false); code != 303 {
			t.Errorf("%s without a login should redirect, got %d", p, code)
		}
	}
	_, page, _ := do("GET", "/services/new", nil, true)
	for _, want := range []string{`data-port="5432"`, `data-port="6379"`, `data-port="27017"`, `data-port="5672"`, "RabbitMQ", "Test connection", "pg_monitor"} {
		if !strings.Contains(page, want) {
			t.Errorf("add form is missing %q", want)
		}
	}

	// validation
	bad := func(form url.Values, want string) {
		t.Helper()
		code, body, _ := do("POST", "/services", form, true)
		if code != 400 || !strings.Contains(body, want) {
			t.Errorf("want 400 containing %q, got %d", want, code)
		}
	}
	bad(url.Values{"name": {"Bad Name"}, "kind": {"redis"}, "host": {"h"}, "port": {"6379"}}, "lowercase")
	bad(url.Values{"name": {"r"}, "kind": {"redis"}, "host": {"bad host!"}, "port": {"6379"}}, "host name")
	bad(url.Values{"name": {"r"}, "kind": {"redis"}, "host": {"h"}, "port": {"99999"}}, "port")
	bad(url.Values{"name": {"r"}, "kind": {"nosuch"}, "host": {"h"}, "port": {"1"}}, "Choose a kind")
	bad(url.Values{"name": {"w"}, "kind": {"http"}, "p_url": {"ftp://x"}}, "http://")
	bad(url.Values{"name": {"r"}, "kind": {"redis"}, "host": {"h"}, "port": {"6379"}, "t_mem_pct": {"lots"}}, "number")

	// an HTTP service that needs basic auth, to prove the password is used but never shown
	secretServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "monitor" || p != "s3cr3t-pass" {
			w.WriteHeader(401)
		}
	}))
	defer secretServer.Close()
	code, _, loc := do("POST", "/services", url.Values{"name": {"gopherpost"}, "kind": {"http"}, "p_url": {secretServer.URL}, "username": {"monitor"},
		"password": {"s3cr3t-pass"}, "enabled": {"on"}, "t_latency_ms": {"1500"}, "fail_checks": {"3"}}, true)
	if code != 303 || !strings.HasPrefix(loc, "/services/") {
		t.Fatalf("create failed: %d %s", code, loc)
	}
	list, _ := srv.Store.ListServices(context.Background())
	if len(list) != 1 || list[0].Params["t_latency_ms"] != "1500" || list[0].Params["fail_checks"] != "3" {
		t.Fatalf("settings not stored: %+v", list)
	}
	if strings.Contains(string(list[0].Password), "s3cr3t-pass") || len(list[0].Password) == 0 {
		t.Fatal("the password must be stored encrypted")
	}
	id := list[0].ID
	idStr := strconv.FormatInt(id, 10)
	if code, _, _ := do("POST", "/services", url.Values{"name": {"gopherpost"}, "kind": {"tcp"}, "host": {"h"}, "port": {"1"}}, true); code != 400 {
		t.Fatal("a duplicate name must be refused")
	}

	// test connection (nothing saved): the saved password is used when editing with it left empty
	code, frag, _ := do("POST", "/services/test", url.Values{"id": {idStr}, "kind": {"http"}, "p_url": {secretServer.URL}, "username": {"monitor"}}, true)
	if code != 200 || !strings.Contains(frag, "Connected") || strings.Contains(frag, "s3cr3t-pass") {
		t.Fatalf("test with the saved password should connect without leaking it: %d %s", code, frag)
	}
	_, frag, _ = do("POST", "/services/test", url.Values{"kind": {"http"}, "p_url": {secretServer.URL}, "username": {"monitor"}, "password": {"wrong"}}, true)
	if !strings.Contains(frag, "Could not connect") || !strings.Contains(frag, "401") {
		t.Fatalf("a wrong password must be reported: %s", frag)
	}
	_, frag, _ = do("POST", "/services/test", url.Values{"kind": {"tcp"}, "host": {"127.0.0.1"}, "port": {"1"}}, true)
	if !strings.Contains(frag, "Could not connect") {
		t.Fatalf("a closed port must be reported: %s", frag)
	}
	_, frag, _ = do("POST", "/services/test", url.Values{"kind": {"redis"}, "host": {"bad host"}, "port": {"6379"}}, true)
	if !strings.Contains(frag, "host name") {
		t.Fatalf("bad input is explained in the test result: %s", frag)
	}

	// three checks give the detail page real data
	svc, _ := srv.Store.ServiceByID(context.Background(), id)
	for i := 0; i < 3; i++ {
		srv.Sampler.CheckService(context.Background(), *svc)
		time.Sleep(20 * time.Millisecond)
	}
	_, detail, _ := do("GET", "/services/"+idStr+"?range=1h", nil, true)
	for _, want := range []string{"gopherpost", `class="badge ok">Up<`, "chartsvg", "Response time", `href="/services/` + idStr + `?range=7d"`, `aria-current="true">1 hour`} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail page is missing %q", want)
		}
	}
	for _, body := range []string{detail, page} {
		if strings.Contains(body, "s3cr3t-pass") {
			t.Fatal("the password leaked into a page")
		}
	}
	if _, d2, _ := do("GET", "/services/"+idStr+"?range=bogus", nil, true); !strings.Contains(d2, `aria-current="true">24 hours`) {
		t.Error("an unknown range should fall back to 24 hours")
	}
	if code, _, _ := do("GET", "/services/9999", nil, true); code != 404 {
		t.Errorf("an unknown service should be 404, got %d", code)
	}

	// edit: the password is kept when the field is left empty; the kind cannot change
	_, edit, _ := do("GET", "/services/"+idStr+"/edit", nil, true)
	if !strings.Contains(edit, "saved, leave empty to keep") || strings.Contains(edit, "s3cr3t-pass") {
		t.Fatal("the edit form should say a password is saved without showing it")
	}
	do("POST", "/services/"+idStr, url.Values{"kind": {"http"}, "p_url": {secretServer.URL}, "username": {"monitor"}, "t_latency_ms": {"900"}, "enabled": {"on"}}, true)
	after, _ := srv.Store.ServiceByID(context.Background(), id)
	if after.Params["t_latency_ms"] != "900" || len(after.Password) == 0 {
		t.Fatalf("edit should update settings and keep the password: %+v", after.Params)
	}
	if code, body, _ := do("POST", "/services/"+idStr, url.Values{"kind": {"tcp"}, "host": {"h"}, "port": {"1"}}, true); code != 400 || !strings.Contains(body, "cannot be changed") {
		t.Fatalf("changing the kind must be refused: %d", code)
	}
	do("POST", "/services/"+idStr, url.Values{"kind": {"http"}, "p_url": {secretServer.URL}, "clear_password": {"on"}, "enabled": {"on"}}, true)
	if cleared, _ := srv.Store.ServiceByID(context.Background(), id); len(cleared.Password) != 0 {
		t.Fatal("the password should be clearable")
	}

	// a service that is down raises an alert that the list page shows
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	_, _, _ = do("POST", "/services", url.Values{"name": {"mongo-main"}, "kind": {"tcp"}, "host": {"127.0.0.1"}, "port": {strconv.Itoa(port)}, "enabled": {"on"}}, true)
	all, _ := srv.Store.ListServices(context.Background())
	for _, v := range all {
		if v.Name == "mongo-main" {
			// creating the service already started a background first check; keep checking until
			// two failures in a row have opened the alert
			for i := 0; i < 20; i++ {
				srv.Sampler.CheckService(context.Background(), v)
				if open, _ := srv.Store.OpenAlertsFor(context.Background(), v.ID); len(open) > 0 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	_, list2, _ := do("GET", "/services", nil, true)
	for _, want := range []string{"Active alerts", "mongo-main", "is down", `class="badge failed">Down<`, "Email alerts are not set up"} {
		if !strings.Contains(list2, want) {
			t.Errorf("services page is missing %q", want)
		}
	}

	// check now and delete
	if code, _, _ := do("POST", "/services/"+idStr+"/check", nil, true); code != 303 {
		t.Errorf("check now: %d", code)
	}
	if code, _, _ := do("POST", "/services/"+idStr+"/delete", nil, true); code != 303 {
		t.Errorf("delete: %d", code)
	}
	if gone, _ := srv.Store.ServiceByID(context.Background(), id); gone != nil {
		t.Fatal("the service should be gone")
	}
	rows, _ := srv.Store.ListAudit(context.Background(), 50, 0)
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.Action)
		if strings.Contains(r.Target, "s3cr3t") {
			t.Fatal("a secret reached the audit log")
		}
	}
	joined := strings.Join(actions, " ")
	for _, want := range []string{"service.create", "service.test", "service.update", "service.delete"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit is missing %s (have %s)", want, joined)
		}
	}
}

func TestSuggestedServices(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	srv.Sampler = &monitor.Sampler{Store: srv.Store, Box: srv.Box}
	srv.ExpectedServices = []config.ExpectedService{
		{Name: "redis-main", Kind: "redis", Host: "127.0.0.1", Port: 6379, Note: "Host Redis."},
		{Name: "gotenberg", Kind: "http", Host: "127.0.0.1", Port: 3025, Params: map[string]string{"url": "http://127.0.0.1:3025/health"}, Note: "PDF."},
	}
	c := client()
	get := func(path string) string {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.AddCookie(cookie)
		r, _ := c.Do(req)
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	page := get("/services")
	if !strings.Contains(page, "Suggested for this server") || !strings.Contains(page, "redis-main") || !strings.Contains(page, "/services/new?preset=gotenberg") {
		t.Fatalf("suggestions should be listed:\n%s", page)
	}
	form := get("/services/new?preset=gotenberg")
	for _, want := range []string{`value="gotenberg"`, `value="3025"`, `value="http://127.0.0.1:3025/health"`, `<option value="http" data-port="0" selected>`} {
		if !strings.Contains(form, want) {
			t.Errorf("the preset should pre-fill the form (missing %s)", want)
		}
	}
	if strings.Contains(get("/services/new?preset=nope"), `value="gotenberg"`) {
		t.Error("an unknown preset must not pre-fill anything")
	}
	// adding one removes it from the suggestions
	req, _ := http.NewRequest("POST", ts.URL+"/services", strings.NewReader(url.Values{"name": {"redis-main"}, "kind": {"redis"}, "host": {"127.0.0.1"}, "port": {"6379"}, "enabled": {"on"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	c.Do(req)
	if after := get("/services"); strings.Contains(after, "preset=redis-main") || !strings.Contains(after, "preset=gotenberg") {
		t.Error("an added service should leave the suggestions, the others stay")
	}
}

func TestRequestLog(t *testing.T) {
	_, _, srv, cookie := setupLoggedIn(t)
	hub := logs.NewHub(500)
	old := log.Writer()
	log.SetOutput(hub)
	defer log.SetOutput(old)

	serve := func(mode string) *httptest.Server {
		srv.RequestLog = mode
		h, err := srv.Handler()
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		return s
	}
	lines := func() []string {
		recent, _, cancel := hub.Subscribe(500)
		cancel()
		var out []string
		for _, l := range recent {
			if strings.HasPrefix(l[strings.Index(l, " ")+1:][strings.Index(l[strings.Index(l, " ")+1:], " ")+1:], "http: ") || strings.Contains(l, " http: ") {
				out = append(out, l)
			}
		}
		return out
	}
	c := client()
	call := func(ts *httptest.Server, method, path string, form url.Values, headers map[string]string, auth bool) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if auth {
			req.AddCookie(cookie)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r.StatusCode
	}
	has := func(substr string) bool {
		for _, l := range lines() {
			if strings.Contains(l, substr) {
				return true
			}
		}
		return false
	}

	// quiet (the default): page requests are logged, background noise is not
	quiet := serve("quiet")
	call(quiet, "GET", "/", nil, nil, true)
	call(quiet, "GET", "/static/app.css", nil, nil, false)
	call(quiet, "POST", "/apps", url.Values{"name": {"demo"}, "repo_url": {"https://example.com/r.git"}, "branch": {"main"}, "compose_file": {"docker-compose.yml"}}, nil, true)
	call(quiet, "GET", "/apps/demo/status", nil, map[string]string{"HX-Request": "true"}, true)  // htmx polling
	call(quiet, "GET", "/apps/ghost/status", nil, map[string]string{"HX-Request": "true"}, true) // a failing poll is still logged
	call(quiet, "POST", "/login", url.Values{"username": {"admin"}, "password": {"hunter2-secret"}, "code": {"000000"}}, nil, false)
	call(quiet, "GET", "/nope", nil, nil, true)
	call(quiet, "POST", "/apps/demo/env", url.Values{"key": {"DB_PASSWORD"}, "value": {"s3cr3t-value"}, "secret": {"on"}}, nil, true)
	call(quiet, "GET", "/apps/demo?msg=flash-secret-text&err=boom", nil, map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.1"}, true)

	if !has("http: GET / 200") || !has("user=admin ip=127.0.0.1") {
		t.Fatalf("a page request should be logged with its user:\n%s", strings.Join(lines(), "\n"))
	}
	if !has("http: POST /apps 303") {
		t.Error("a POST that redirects should be logged with 303")
	}
	if has("/static/app.css") {
		t.Error("static files must not be logged in quiet mode")
	}
	if has("GET /apps/demo/status") {
		t.Error("successful htmx polling must not be logged in quiet mode")
	}
	if !has("GET /apps/ghost/status 404") {
		t.Error("a failing background request should still be logged")
	}
	if !has("http: POST /login 401") || !has("POST /login 401") {
		t.Error("a failed login should be logged")
	}
	if !has("http: GET /nope 404") {
		t.Error("a 404 should be logged")
	}
	if !has("xff=203.0.113.9") {
		t.Error("the forwarded address should be shown, marked as such")
	}
	all := strings.Join(lines(), "\n")
	for _, secret := range []string{"hunter2-secret", "s3cr3t-value", "flash-secret-text", "boom", "msg=", "?"} {
		if strings.Contains(all, secret) {
			t.Fatalf("the request log must never contain %q:\n%s", secret, all)
		}
	}
	if !strings.Contains(all, "http: GET /apps/demo 200") {
		t.Error("the path is logged without its query string")
	}

	// all: everything, including static files and polling
	before := len(lines())
	every := serve("all")
	call(every, "GET", "/static/app.css", nil, nil, false)
	call(every, "GET", "/apps/demo/status", nil, map[string]string{"HX-Request": "true"}, true)
	if got := lines()[before:]; len(got) != 2 {
		t.Errorf("mode all should log both requests, got %v", got)
	}

	// off: nothing
	before = len(lines())
	off := serve("off")
	call(off, "GET", "/", nil, nil, true)
	if len(lines()) != before {
		t.Error("mode off must log nothing")
	}
}

func TestChangedUsernameNeedsThePasswordAgain(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	srv.Sampler = &monitor.Sampler{Store: srv.Store, Box: srv.Box}
	c := client()
	do := func(path string, form url.Values) (int, string) {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	do("/services", url.Values{"name": {"pg"}, "kind": {"http"}, "p_url": {"http://127.0.0.1:1/x"}, "username": {"dom"}, "password": {"old-secret"}, "enabled": {"on"}})
	list, _ := srv.Store.ListServices(context.Background())
	id := strconv.FormatInt(list[0].ID, 10)
	same := func() url.Values {
		return url.Values{"id": {id}, "kind": {"http"}, "p_url": {"http://127.0.0.1:1/x"}, "username": {"dom"}, "enabled": {"on"}}
	}

	// unchanged settings and an empty password field keep the saved password
	if code, _ := do("/services/"+id, same()); code != 303 {
		t.Fatalf("an edit that changes nothing sensitive should save: %d", code)
	}
	// a new username with the password left empty must not silently reuse the old one
	changed := same()
	changed.Set("username", "berth_monitor")
	if code, body := do("/services/"+id, changed); code != 400 || !strings.Contains(body, "type the password again") {
		t.Fatalf("a changed username without a password must be refused: %d", code)
	}
	if after, _ := srv.Store.ServiceByID(context.Background(), list[0].ID); after.Username != "dom" {
		t.Fatal("nothing should have been saved")
	}
	// the Test connection button follows the same rule
	if _, frag := do("/services/test", changed); !strings.Contains(frag, "type the password again") {
		t.Fatalf("testing a changed username with no password must say so: %s", frag)
	}
	// typing the password makes it work, and the new password is used
	changed.Set("password", "new-secret")
	if code, _ := do("/services/"+id, changed); code != 303 {
		t.Fatalf("a changed username with a password should save: %d", code)
	}
	after, _ := srv.Store.ServiceByID(context.Background(), list[0].ID)
	if plain, _ := srv.Box.Open(after.Password); after.Username != "berth_monitor" || plain != "new-secret" {
		t.Fatalf("the new username and password should both be stored: %s %q", after.Username, plain)
	}
}

func TestDiscoverOffersExistingFolderForRenamedRepo(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	srv.Expected = []config.ExpectedApp{{Name: "kite360-web", Title: "Kite360 web", Repo: "https://github.com/dominickasanga/kite360",
		RepoAliases: []string{"https://github.com/dominickasanga/proworkwave_erp"}, Branch: "main", Subfolder: "app/tams-nuxt", ComposeFile: "docker-compose.yml", FolderNames: []string{"kite360"}}}
	root := t.TempDir()
	dir := filepath.Join(root, "Kite360")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:dominickasanga/proworkwave_erp.git\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)

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
	do("POST", "/discover/paths/delete", url.Values{"path": {"/var/www"}})
	do("POST", "/discover/paths/delete", url.Values{"path": {"/mnt/blockvolume"}})
	do("POST", "/discover/paths", url.Values{"path": {root}})

	_, page, _ := do("GET", "/discover", nil)
	if !strings.Contains(page, `value="existing"`) || strings.Contains(page, "different repository") || strings.Contains(page, "not the expected repository") {
		t.Fatal("a folder using the repository's old name should be offered as the existing folder")
	}
	if _, _, loc := do("POST", "/discover/register", url.Values{"expected": {"kite360-web"}, "mode": {"existing"}}); !strings.HasPrefix(loc, "/apps/kite360-web") {
		t.Fatalf("registering from the renamed-repo folder failed: %q", loc)
	}
	app, _ := srv.Store.AppByName(context.Background(), "kite360-web")
	if app == nil || app.SourceMode != "existing" || app.SourcePath != dir || app.RepoURL != "git@github.com:dominickasanga/proworkwave_erp.git" {
		t.Fatalf("the app should use the folder and the remote that already works there: %+v", app)
	}
}

func TestGroupServices(t *testing.T) {
	card := func(name, kind string, up bool, alerts int) serviceCard {
		return serviceCard{Service: store.Service{Name: name, Kind: kind, Enabled: true}, State: &store.ServiceState{Up: up}, Alerts: alerts}
	}
	groups := groupServices([]serviceCard{
		card("probe", "tcp", true, 0), card("api", "http", true, 0), card("pg", "postgres", false, 1),
		card("cache", "redis", true, 0), card("mystery", "something-new", true, 0), card("mq", "rabbitmq", true, 0),
	})
	var keys []string
	for _, g := range groups {
		keys = append(keys, g.Key)
	}
	if got := strings.Join(keys, ","); got != "databases,messaging,web,ports" {
		t.Fatalf("sections or their order: %s", got)
	}
	db, last := groups[0], groups[3]
	if len(db.Cards) != 2 || db.Cards[0].Service.Name != "pg" || db.Down != 1 || db.Alerts != 1 {
		t.Errorf("databases and caches share one section, with its problems counted: %+v", db)
	}
	if len(last.Cards) != 2 {
		t.Errorf("an unknown kind belongs in the last section: %+v", last.Cards)
	}
	if only := groupServices([]serviceCard{card("api", "http", true, 0)}); len(only) != 1 || only[0].Key != "web" {
		t.Errorf("empty sections are left out: %+v", only)
	}
}

func TestServicesPageGroupsAndDeployTerminal(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	ctx := context.Background()
	for _, v := range []store.Service{
		{Name: "probe", Kind: "tcp", Host: "127.0.0.1", Port: 9, Enabled: true},
		{Name: "api", Kind: "http", Enabled: true, Params: map[string]string{"url": "http://127.0.0.1:1/"}},
		{Name: "mq", Kind: "rabbitmq", Host: "127.0.0.1", Port: 5672, Enabled: true},
		{Name: "pg", Kind: "postgres", Host: "127.0.0.1", Port: 5432, Enabled: true},
	} {
		if _, err := srv.Store.CreateService(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	c := client()
	get := func(path string) string {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.AddCookie(cookie)
		r, err := c.Do(req)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, err, r)
		}
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	page := get("/services")
	last := -1
	for _, key := range []string{"databases", "messaging", "web", "ports"} {
		if !strings.Contains(page, `href="#grp-`+key+`"`) {
			t.Errorf("no jump link to the %s section", key)
		}
		i := strings.Index(page, `id="grp-`+key+`"`)
		if i < 0 || i < last {
			t.Fatalf("section %s missing or out of order", key)
		}
		last = i
	}
	for _, icon := range []string{"#database", "#message-square", "#globe", "#plug"} {
		if !strings.Contains(page, icon) {
			t.Errorf("the page does not use the %s icon", icon)
		}
	}
	// every icon the page asks for exists in the sprite
	sprite := get("/static/icons.svg")
	for _, id := range []string{"database", "message-square", "globe", "plug", "maximize", "minimize"} {
		if !strings.Contains(sprite, `id="`+id+`"`) {
			t.Errorf("icons.svg has no %s", id)
		}
	}

	// the deploy output sits in a terminal block with a full screen control outside the refreshed part
	req, _ := http.NewRequest("POST", ts.URL+"/apps", strings.NewReader(url.Values{"name": {"demo"}, "repo_url": {"https://example.com/r.git"}, "branch": {"main"}, "compose_file": {"docker-compose.yml"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	if r, err := c.Do(req); err != nil || r.StatusCode != 303 {
		t.Fatalf("create app: %v %v", err, r)
	}
	app, _ := srv.Store.AppByName(ctx, "demo")
	did, err := srv.Store.StartDeploy(ctx, app.ID, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	appPage := get("/apps/demo")
	termAt, btnAt, bodyAt := strings.Index(appPage, "data-term>"), strings.Index(appPage, "data-term-full"), strings.Index(appPage, `class="termbody" hx-get="/deploys/`+strconv.FormatInt(did, 10)+`/log"`)
	if termAt < 0 || btnAt < termAt || bodyAt < btnAt {
		t.Errorf("deploy terminal: block at %d, full screen button at %d, refreshed part at %d", termAt, btnAt, bodyAt)
	}
	// the script and styles carry the pieces the markup relies on, in both themes
	js, css := get("/static/app.js"), get("/static/app.css")
	for _, want := range []string{"berthLog", "data-term-full", "'Escape'"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	for _, want := range []string{".ll-err{color:var(--bad)}", ".ll-warn{color:var(--warn)}", ".ll-ok{color:var(--ok)}", ".term.full{position:fixed"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}
	for _, v := range []string{"--bad:", "--warn:", "--ok:"} {
		if strings.Count(css, v) < 2 {
			t.Errorf("%s must be defined for both the light and the dark theme", v)
		}
	}
}
