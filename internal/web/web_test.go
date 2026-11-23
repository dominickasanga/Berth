package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
	"github.com/dominickasanga/berth/internal/store"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

func setup(t *testing.T) (*httptest.Server, string) {
	dsn := os.Getenv("BERTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BERTH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	st.Pool.Exec(ctx, `TRUNCATE admins, sessions, apps, app_env, deploys, audit_log RESTART IDENTITY CASCADE`)
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
	return ts, key.Secret()
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
