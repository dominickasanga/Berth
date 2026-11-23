// Package web serves Berth's UI (html/template + htmx) and enforces admin login.
package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dominickasanga/berth/config"
	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
	"github.com/dominickasanga/berth/internal/discover"
	"github.com/dominickasanga/berth/internal/logs"
	"github.com/dominickasanga/berth/internal/monitor"
	"github.com/dominickasanga/berth/internal/store"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static/*
var staticFS embed.FS

const cookieName = "berth_session"

type Server struct {
	Store        *store.Store
	Box          *crypt.Box
	Deploy       *deploy.Engine
	CookieSecure bool
	Expected     []config.ExpectedApp
	// BasePath is the URL prefix Berth is served under, e.g. "/_berth" ("" = the site root).
	// The proxy in front must pass the prefix through unchanged.
	BasePath string
	// Logs holds Berth's own recent log lines for the Logs page (nil disables the page).
	Logs *logs.Hub
	// Sampler checks the supporting services; MailOn says whether alert emails are configured.
	Sampler *monitor.Sampler
	MailOn  bool
	// Version is shown in the page footer so you can see which build is running.
	Version string
	// RequestLog is "quiet" (default), "all" or "off": see logRequests.
	RequestLog string
	// ExpectedServices are the services suggested on the Services page.
	ExpectedServices []config.ExpectedService

	tpl       map[string]*template.Template
	dummyHash []byte
	mu        sync.Mutex
	fails     map[string]*failState
}

type failState struct {
	n     int
	until time.Time
}

type ctxKey struct{}

func (s *Server) Handler() (http.Handler, error) {
	s.fails = map[string]*failState{}
	s.dummyHash, _ = bcrypt.GenerateFromPassword([]byte("berth"), bcrypt.DefaultCost)
	s.tpl = map[string]*template.Template{}
	static, _ := fs.Sub(staticFS, "static")
	ver := staticVersion(static)
	funcs := template.FuncMap{
		"v":       func() string { return ver },
		"base":    func() string { return s.BasePath },
		"version": func() string { return s.Version },
		"icon": func(name string) template.HTML { // one icon from the cached sprite
			return template.HTML(`<svg class="i" aria-hidden="true" focusable="false"><use href="` + s.BasePath + `/static/icons.svg?v=` + ver + `#` + name + `"/></svg>`)
		},
	}
	entries, err := tplFS.ReadDir("templates") // every template file except the layout and fragments is a page
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		page := strings.TrimSuffix(e.Name(), ".html")
		if page == "layout" || page == "fragments" {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(tplFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		s.tpl[page] = t
	}
	frag, err := template.New("").Funcs(funcs).ParseFS(tplFS, "templates/fragments.html")
	if err != nil {
		return nil, err
	}
	s.tpl["fragments"] = frag

	mux := http.NewServeMux()
	mux.Handle("GET /static/", cacheStatic(http.StripPrefix("/static/", http.FileServer(http.FS(static)))))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.auth(s.logout))
	mux.HandleFunc("GET /{$}", s.auth(s.appsPage))
	mux.HandleFunc("GET /services", s.auth(s.servicesPage))
	mux.HandleFunc("GET /services/new", s.auth(s.serviceNewPage))
	mux.HandleFunc("POST /services", s.auth(s.serviceCreate))
	mux.HandleFunc("POST /services/test", s.auth(s.serviceTest))
	mux.HandleFunc("GET /services/{id}", s.auth(s.serviceDetail))
	mux.HandleFunc("GET /services/{id}/edit", s.auth(s.serviceEditPage))
	mux.HandleFunc("POST /services/{id}", s.auth(s.serviceUpdate))
	mux.HandleFunc("POST /services/{id}/delete", s.auth(s.serviceDelete))
	mux.HandleFunc("POST /services/{id}/check", s.auth(s.serviceCheck))
	mux.HandleFunc("GET /audit", s.auth(s.auditPage))
	mux.HandleFunc("GET /logs", s.auth(s.logsPage))
	mux.HandleFunc("GET /logs/stream", s.auth(s.logsStream))
	mux.HandleFunc("GET /discover", s.auth(s.discoverPage))
	mux.HandleFunc("POST /discover/paths", s.auth(s.discoverAddPath))
	mux.HandleFunc("POST /discover/paths/delete", s.auth(s.discoverDeletePath))
	mux.HandleFunc("POST /discover/choose", s.auth(s.discoverChoose))
	mux.HandleFunc("POST /discover/register", s.auth(s.discoverRegister))
	mux.HandleFunc("POST /discover/add", s.auth(s.discoverAdd))
	mux.HandleFunc("GET /apps/new", s.auth(s.appNewPage))
	mux.HandleFunc("POST /apps", s.auth(s.appCreate))
	mux.HandleFunc("GET /apps/{name}", s.auth(s.appPage))
	mux.HandleFunc("GET /apps/{name}/status", s.auth(s.appStatus))
	mux.HandleFunc("POST /apps/{name}/deploy", s.auth(s.appDeploy))
	mux.HandleFunc("POST /apps/{name}/control", s.auth(s.appControl))
	mux.HandleFunc("POST /apps/{name}/rollback", s.auth(s.appRollback))
	mux.HandleFunc("POST /apps/{name}/source", s.auth(s.appSource))
	mux.HandleFunc("GET /apps/{name}/logs/stream", s.auth(s.logStream))
	mux.HandleFunc("POST /apps/{name}/env", s.auth(s.envSet))
	mux.HandleFunc("POST /apps/{name}/env/delete", s.auth(s.envDelete))
	mux.HandleFunc("GET /deploys/{id}/log", s.auth(s.deployLog))
	h := s.sameOrigin(secureHeaders(mux))
	mode := s.RequestLog
	if mode == "" {
		mode = "quiet"
	}
	if s.BasePath == "" {
		return logRequests(mode, "", h), nil
	}
	root := http.NewServeMux()
	root.Handle(s.BasePath+"/", http.StripPrefix(s.BasePath, h))
	toBase := func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, s.BasePath+"/", http.StatusFound) }
	root.HandleFunc("GET "+s.BasePath, toBase)
	root.HandleFunc("GET /{$}", toBase) // the bare site root points people to Berth
	return logRequests(mode, s.BasePath, root), nil
}

// staticVersion hashes the embedded static files; pages link them as ?v=<hash>.
func staticVersion(static fs.FS) string {
	h := sha256.New()
	fs.WalkDir(static, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(static, path)
			h.Write([]byte(path))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}

// cacheStatic lets browsers keep versioned files for a year (the URL changes when the file does).
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// NormalizeBase validates a base path: "" or "/word" segments of letters, digits, "-", "_", ".".
func NormalizeBase(b string) (string, error) {
	b = strings.TrimSpace(b)
	b = strings.TrimRight(b, "/")
	if b == "" {
		return "", nil
	}
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	if !regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`).MatchString(b) || strings.Contains(b, "..") {
		return "", errors.New("base path may only contain letters, digits, '-', '_', '.' and '/' segments, e.g. /_berth")
	}
	return b, nil
}

func (s *Server) cookiePath() string {
	if s.BasePath == "" {
		return "/"
	}
	return s.BasePath
}

// redirect sends the browser to a path inside Berth (the base path is added).
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, target string) {
	http.Redirect(w, r, s.BasePath+target, http.StatusSeeOther)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects cross-site state changes (CSRF); cookies are also SameSite=Strict.
func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next func(http.ResponseWriter, *http.Request, *store.Admin)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err == nil {
			if a, err := s.Store.SessionAdmin(r.Context(), c.Value); err == nil && a != nil {
				setLoggedUser(r.Context(), a.Username)
				next(w, r, a)
				return
			}
		}
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", s.BasePath+"/login")
			return
		}
		s.redirect(w, r, "/login")
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := "layout"
	if page == "fragments" {
		name = ""
	}
	t := s.tpl[page]
	if t == nil {
		log.Printf("render: no template for page %q", page)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	var err error
	if name == "" {
		err = t.ExecuteTemplate(w, data.(frag).Name, data.(frag).Data)
	} else {
		err = t.ExecuteTemplate(w, name, data)
	}
	if err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

type frag struct {
	Name string
	Data any
}

// ---- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) { s.render(w, "login", nil) }

func (s *Server) tooManyFails(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fails[user]
	return f != nil && f.n >= 5 && time.Now().Before(f.until)
}

func (s *Server) noteFail(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fails[user]
	if f == nil {
		f = &failState{}
		s.fails[user] = f
	}
	f.n++
	f.until = time.Now().Add(10 * time.Minute)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.FormValue("username"))
	fail := func() {
		log.Printf("login failed: user=%q ip=%s", user, r.RemoteAddr)
		s.noteFail(user)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", map[string]string{"Error": "Wrong username, password or code."})
	}
	if s.tooManyFails(user) {
		log.Printf("login refused (too many attempts): user=%q ip=%s", user, r.RemoteAddr)
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login", map[string]string{"Error": "Too many attempts. Try again in 10 minutes."})
		return
	}
	a, err := s.Store.AdminByUsername(r.Context(), user)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	if a == nil {
		bcrypt.CompareHashAndPassword(s.dummyHash, []byte(r.FormValue("password"))) // equalise timing
		fail()
		return
	}
	secret, err := s.Box.Open(a.TOTPSecret)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	pwOK := bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(r.FormValue("password"))) == nil
	codeOK := totp.Validate(strings.TrimSpace(r.FormValue("code")), secret)
	if !pwOK || !codeOK {
		fail()
		return
	}
	token, err := s.Store.NewSession(r.Context(), a.ID, 12*time.Hour)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	s.mu.Lock()
	delete(s.fails, user)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: s.cookiePath(), HttpOnly: true,
		Secure: s.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	setLoggedUser(r.Context(), a.Username)
	s.Store.Audit(r.Context(), a.Username, "login", "")
	s.redirect(w, r, "/")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.Store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: s.cookiePath(), MaxAge: -1})
	s.redirect(w, r, "/login")
}

// ---- apps ----

func (s *Server) appsPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	apps, err := s.Store.ListApps(r.Context())
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	s.render(w, "apps", map[string]any{"Apps": apps, "User": a.Username})
}

func (s *Server) appNewPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	s.render(w, "app_new", map[string]any{"User": a.Username, "Form": store.App{Branch: "main", ComposeFile: "docker-compose.yml"}})
}

func (s *Server) appCreate(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := store.App{
		Name:         strings.TrimSpace(r.FormValue("name")),
		RepoURL:      strings.TrimSpace(r.FormValue("repo_url")),
		Branch:       strings.TrimSpace(r.FormValue("branch")),
		Subfolder:    strings.Trim(strings.TrimSpace(r.FormValue("subfolder")), "/"),
		ComposeFile:  strings.TrimSpace(r.FormValue("compose_file")),
		PredeploySvc: strings.TrimSpace(r.FormValue("predeploy_svc")),
	}
	err := deploy.ValidateApp(app)
	if err == nil {
		err = s.Store.CreateApp(r.Context(), app)
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		msg := "Could not save: " + err.Error()
		if strings.Contains(err.Error(), "apps_name") || strings.Contains(err.Error(), "violates") {
			msg = "Name must be lowercase letters, digits and dashes, and not already used."
		}
		s.render(w, "app_new", map[string]any{"User": a.Username, "Form": app, "Error": msg})
		return
	}
	s.Store.Audit(r.Context(), a.Username, "app.create", app.Name)
	s.redirect(w, r, "/apps/"+app.Name)
}

type envRow struct {
	Key, Value string
	IsSecret   bool
}

type deployRow struct {
	store.Deploy
	CanRollback bool
}

func (s *Server) loadApp(w http.ResponseWriter, r *http.Request) *store.App {
	app, err := s.Store.AppByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, "server error", 500)
		return nil
	}
	if app == nil {
		http.NotFound(w, r)
	}
	return app
}

func (s *Server) appPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	vars, err := s.Store.ListEnv(r.Context(), app.ID)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	rows := make([]envRow, 0, len(vars))
	for _, v := range vars {
		row := envRow{Key: v.Key, IsSecret: v.IsSecret}
		if !v.IsSecret { // secrets are never shown again after saving
			row.Value, _ = s.Box.Open(v.Value)
		}
		rows = append(rows, row)
	}
	latest, _ := s.Store.LatestDeploys(r.Context(), app.ID, 10)
	deploys := make([]deployRow, 0, len(latest))
	seenSuccess := false // the newest success is what is running now; older ones can be rolled back to
	for _, d := range latest {
		row := deployRow{Deploy: d}
		if d.Status == "success" {
			row.CanRollback = seenSuccess && d.ImagesTagged
			seenSuccess = true
		}
		deploys = append(deploys, row)
	}
	s.render(w, "app", map[string]any{"User": a.Username, "App": app, "Env": rows, "Deploys": deploys, "Src": s.sourceOptions(r.Context(), *app), "Flash": r.URL.Query().Get("msg"), "Err": r.URL.Query().Get("err")})
}

func (s *Server) appStatus(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	svcs, err := s.Deploy.Services(*app)
	state := deploy.Summarize(svcs)
	data := map[string]any{"AppName": app.Name, "Services": svcs, "State": state,
		"CanStart": state == "stopped", "CanStop": state == "running", "CanRestart": state == "running"}
	if err != nil {
		data["Err"] = err.Error()
		data["CanStart"], data["CanStop"], data["CanRestart"] = false, false, false
	}
	if last, _ := s.Store.LatestDeploys(r.Context(), app.ID, 1); len(last) == 1 {
		data["Last"] = last[0]
	}
	name := "status"
	if r.URL.Query().Get("view") == "compact" {
		name = "status-compact"
	}
	s.render(w, "fragments", frag{name, data})
}

func (s *Server) appDeploy(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	id, err := s.Deploy.Start(r.Context(), *app)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, deploy.ErrBusy) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "app.deploy", app.Name)
	s.redirect(w, r, "/apps/"+app.Name+"?msg=deploy+"+strconv.FormatInt(id, 10)+"+started")
}

func (s *Server) appControl(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	action := r.FormValue("action")
	out, err := s.Deploy.Control(*app, action)
	s.Store.Audit(r.Context(), a.Username, "app."+action, app.Name)
	if err != nil {
		detail := strings.TrimSpace(out)
		log.Printf("%s %s failed: %v\n%s", action, app.Name, err, detail)
		if len(detail) > 1500 {
			detail = "…" + detail[len(detail)-1500:]
		}
		s.redirect(w, r, "/apps/"+app.Name+"?err="+url.QueryEscape(action+" failed ("+err.Error()+"):\n"+detail))
		return
	}
	s.redirect(w, r, "/apps/"+app.Name+"?msg="+url.QueryEscape(action+" ok"))
}

func (s *Server) envSet(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	key, val := strings.TrimSpace(r.FormValue("key")), r.FormValue("value")
	if strings.ContainsAny(val, "\r\n\x00") {
		http.Error(w, "value cannot contain newlines", http.StatusBadRequest)
		return
	}
	sealed, err := s.Box.Seal(val)
	if err == nil {
		err = s.Store.SetEnv(r.Context(), app.ID, key, sealed, r.FormValue("secret") == "on")
	}
	if err != nil {
		http.Error(w, "could not save (key must look like MY_VAR)", http.StatusBadRequest)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "env.set", app.Name+":"+key)
	s.redirect(w, r, "/apps/"+app.Name+"?msg=saved+"+url.QueryEscape(key)+"+(applies+on+next+deploy)")
}

func (s *Server) envDelete(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	key := r.FormValue("key")
	s.Store.DeleteEnv(r.Context(), app.ID, key)
	s.Store.Audit(r.Context(), a.Username, "env.delete", app.Name+":"+key)
	s.redirect(w, r, "/apps/"+app.Name)
}

func (s *Server) deployLog(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := s.Store.DeployByID(context.Background(), id)
	if err != nil || d == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "fragments", frag{"log", d})
}

func (s *Server) appRollback(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	target, _ := strconv.ParseInt(r.FormValue("deploy_id"), 10, 64)
	id, err := s.Deploy.Rollback(r.Context(), *app, target)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, deploy.ErrBusy) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "app.rollback", app.Name+" to deploy "+strconv.FormatInt(target, 10))
	s.redirect(w, r, "/apps/"+app.Name+"?msg=rollback+"+strconv.FormatInt(id, 10)+"+started")
}

// logStream sends `compose logs -f` as server-sent events until the browser disconnects.
func (s *Server) logStream(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // SSE must not be buffered by a proxy
	fl.Flush()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(s.Deploy.LogStream(r.Context(), *app, pw)) }()
	defer pr.Close()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		fmt.Fprintf(w, "data: %s\n\n", strings.TrimRight(sc.Text(), "\r"))
		fl.Flush()
	}
	fmt.Fprint(w, "event: end\ndata: log stream ended\n\n")
	fl.Flush()
}

// ---- discovery ----

type discoverRow struct {
	App         config.ExpectedApp
	Copies      []discover.Candidate
	Chosen      string // path of the copy that will be used
	NeedsChoice bool
	Registered  bool
	// "Use the existing folder" is offered only for a folder that is the expected repo on a branch.
	CanUseExisting bool
	ExistingWhy    string // why it is not offered
}

// existingOK says whether Berth can safely run an app straight from this discovered folder.
func existingOK(c *discover.Candidate) (bool, string) {
	switch {
	case c == nil:
		return false, "no copy was found on this server"
	case !c.HasGit:
		return false, "the folder is not a git checkout"
	case !c.RemoteMatch:
		return false, "the folder's git remote is not the expected repository"
	case c.Branch == "":
		return false, "the folder is not on a branch (detached HEAD)"
	}
	return true, ""
}

func (s *Server) scan(ctx context.Context) (paths []string, found map[string][]discover.Candidate, notes []discover.PathNote, err error) {
	if paths, err = s.Store.ListScanPaths(ctx); err != nil {
		return
	}
	found, notes = discover.Scan(paths, s.Expected)
	return
}

func (s *Server) discoverPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	paths, found, notes, err := s.scan(r.Context())
	choices, err2 := s.Store.Choices(r.Context())
	apps, err3 := s.Store.ListApps(r.Context())
	if err != nil || err2 != nil || err3 != nil {
		http.Error(w, "server error", 500)
		return
	}
	registered := map[string]bool{}
	for _, ap := range apps {
		registered[ap.Name] = true
	}
	rows := make([]discoverRow, 0, len(s.Expected))
	for _, x := range s.Expected {
		c, need := discover.Resolve(found[x.Name], choices[x.Name])
		row := discoverRow{App: x, Copies: found[x.Name], NeedsChoice: need, Registered: registered[x.Name]}
		if c != nil {
			row.Chosen = c.Path
		}
		row.CanUseExisting, row.ExistingWhy = existingOK(c)
		if need {
			row.ExistingWhy = "flag which copy to use first"
		}
		rows = append(rows, row)
	}
	s.render(w, "discover", map[string]any{"User": a.Username, "Paths": paths, "Notes": notes, "Rows": rows,
		"Others": discover.ScanOthers(paths, s.Expected, registeredPaths(apps)),
		"Flash":  r.URL.Query().Get("msg"), "Error": r.URL.Query().Get("err")})
}

func (s *Server) discoverRedirect(w http.ResponseWriter, r *http.Request, key, msg string) {
	s.redirect(w, r, "/discover?"+key+"="+url.QueryEscape(msg))
}

func (s *Server) discoverAddPath(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	p := strings.TrimSpace(r.FormValue("path"))
	if p == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		s.discoverRedirect(w, r, "err", "Enter an absolute folder path such as /var/www")
		return
	}
	p = filepath.Clean(p)
	if err := s.Store.AddScanPath(r.Context(), p); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "scanpath.add", p)
	s.discoverRedirect(w, r, "msg", "Added "+p)
}

func (s *Server) discoverDeletePath(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	p := r.FormValue("path")
	s.Store.DeleteScanPath(r.Context(), p)
	s.Store.Audit(r.Context(), a.Username, "scanpath.delete", p)
	s.discoverRedirect(w, r, "msg", "Removed "+p)
}

func (s *Server) expectedByName(name string) *config.ExpectedApp {
	for i := range s.Expected {
		if s.Expected[i].Name == name {
			return &s.Expected[i]
		}
	}
	return nil
}

// discoverChoose flags which on-disk copy to use; the path must be a copy found right now.
func (s *Server) discoverChoose(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	x := s.expectedByName(r.FormValue("expected"))
	if x == nil {
		http.NotFound(w, r)
		return
	}
	_, found, _, err := s.scan(r.Context())
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	path := r.FormValue("path")
	for _, c := range found[x.Name] {
		if c.Path == path {
			s.Store.SetChoice(r.Context(), x.Name, path)
			s.Store.Audit(r.Context(), a.Username, "discover.choose", x.Name+" -> "+path)
			s.discoverRedirect(w, r, "msg", "Using "+path+" for "+x.Title)
			return
		}
	}
	s.discoverRedirect(w, r, "err", "That folder was not found in this scan; nothing was changed.")
}

func (s *Server) discoverRegister(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	x := s.expectedByName(r.FormValue("expected"))
	if x == nil {
		http.NotFound(w, r)
		return
	}
	_, found, _, err := s.scan(r.Context())
	choices, err2 := s.Store.Choices(r.Context())
	if err != nil || err2 != nil {
		http.Error(w, "server error", 500)
		return
	}
	mode := r.FormValue("mode")
	if mode != "existing" && mode != "clone" {
		s.discoverRedirect(w, r, "err", "Choose how to run "+x.Title+": use the existing folder, or clone a new copy.")
		return
	}
	app := store.App{Name: x.Name, RepoURL: x.Repo, Branch: x.Branch, Subfolder: x.Subfolder,
		ComposeFile: x.ComposeFile, PredeploySvc: x.PredeployService, Monitor: x.Monitor, SourceMode: mode,
		ComposeProject: x.ComposeProject}
	c, need := discover.Resolve(found[x.Name], choices[x.Name])
	if mode == "existing" {
		if need {
			s.discoverRedirect(w, r, "err", x.Title+" was found in more than one place. Flag the copy to use first.")
			return
		}
		if ok, why := existingOK(c); !ok {
			s.discoverRedirect(w, r, "err", "Cannot use the existing folder: "+why+". Clone a new copy instead.")
			return
		}
		app.SourcePath, app.RepoURL, app.Branch = c.Path, c.Remote, c.Branch // the folder is the source of truth
	} else if c != nil { // clone: Berth makes its own copy; the folder is only remembered for reference
		app.SourcePath = c.Path
		if c.RemoteMatch { // the remote already works on this server (e.g. its SSH form)
			app.RepoURL = c.Remote
			if c.Branch != "" && !strings.HasPrefix(c.Branch, "-") {
				app.Branch = c.Branch
			}
		}
	}
	err = deploy.ValidateApp(app)
	if err == nil {
		err = s.Store.CreateApp(r.Context(), app)
	}
	if err != nil {
		s.discoverRedirect(w, r, "err", "Could not register "+x.Title+": already registered or invalid settings.")
		return
	}
	s.Store.Audit(r.Context(), a.Username, "app.create", app.Name+" (discovered)")
	s.redirect(w, r, "/apps/"+app.Name+"?msg="+url.QueryEscape("registered from discovery"))
}

// existingCandidate finds the on-disk folder an app could run from, and says why not if none fits.
func (s *Server) existingCandidate(ctx context.Context, app store.App) (*discover.Candidate, string) {
	if x := s.expectedByName(app.Name); x != nil {
		paths, err := s.Store.ListScanPaths(ctx)
		choices, err2 := s.Store.Choices(ctx)
		if err != nil || err2 != nil {
			return nil, "could not read the scan settings"
		}
		found, _ := discover.Scan(paths, []config.ExpectedApp{*x})
		c, need := discover.Resolve(found[x.Name], choices[x.Name])
		if need {
			return nil, "it was found in more than one place; flag one copy on the Discover page first"
		}
		if ok, why := existingOK(c); !ok {
			return nil, why
		}
		return c, ""
	}
	if app.SourcePath == "" {
		return nil, "this app was added by hand and no existing folder is known for it"
	}
	c := discover.Inspect(app.SourcePath, app.RepoURL)
	if ok, why := existingOK(&c); !ok {
		return nil, why
	}
	return &c, ""
}

type sourceOptions struct {
	CanUseExisting bool
	Path, Why      string
}

func (s *Server) sourceOptions(ctx context.Context, app store.App) sourceOptions {
	c, why := s.existingCandidate(ctx, app)
	if c == nil {
		return sourceOptions{Why: why}
	}
	return sourceOptions{CanUseExisting: true, Path: c.Path}
}

// appSource switches an already-registered app between its own clone and an existing folder.
func (s *Server) appSource(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	back := func(key, msg string) {
		s.redirect(w, r, "/apps/"+app.Name+"?"+key+"="+url.QueryEscape(msg))
	}
	mode := r.FormValue("mode")
	if mode != "existing" && mode != "clone" {
		back("err", "Choose how to run this app: use the existing folder, or a Berth clone.")
		return
	}
	if mode == app.SourceMode {
		back("msg", "Already running that way; nothing changed.")
		return
	}
	updated := *app
	updated.SourceMode = mode
	if mode == "existing" {
		c, why := s.existingCandidate(r.Context(), *app)
		if c == nil {
			back("err", "Cannot use the existing folder: "+why+".")
			return
		}
		updated.SourcePath, updated.RepoURL, updated.Branch = c.Path, c.Remote, c.Branch
	}
	if err := deploy.ValidateApp(updated); err != nil {
		back("err", "Cannot switch: "+err.Error())
		return
	}
	err := s.Deploy.Idle(app.ID, func() error {
		return s.Store.UpdateAppSource(r.Context(), app.ID, mode, updated.SourcePath, updated.RepoURL, updated.Branch)
	})
	if errors.Is(err, deploy.ErrBusy) {
		back("err", "A deploy is running for this app. Try again when it has finished.")
		return
	}
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "app.source", app.Name+" -> "+mode)
	msg := "Now running from the existing folder. Deploy to apply it."
	if mode == "clone" {
		msg = "Now using a Berth-managed clone. Deploy to create it; until then Start, Stop and Logs may not work."
	}
	back("msg", msg)
}

// registeredPaths are folders already used by a registered app (they are not "other" apps).
func registeredPaths(apps []store.App) map[string]bool {
	m := map[string]bool{}
	for _, a := range apps {
		if a.SourcePath != "" {
			m[a.SourcePath] = true
		}
	}
	return m
}

var appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// discoverAdd registers a folder from "Other apps found". The folder and compose file must be
// ones the scan finds right now, so the form cannot point Berth at an arbitrary path.
func (s *Server) discoverAdd(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	paths, err := s.Store.ListScanPaths(r.Context())
	apps, err2 := s.Store.ListApps(r.Context())
	if err != nil || err2 != nil {
		http.Error(w, "server error", 500)
		return
	}
	var folder *discover.Other
	others := discover.ScanOthers(paths, s.Expected, registeredPaths(apps))
	for i := range others {
		if others[i].Path == r.FormValue("path") {
			folder = &others[i]
		}
	}
	if folder == nil {
		s.discoverRedirect(w, r, "err", "That folder was not found in this scan, or it is already added; nothing was changed.")
		return
	}
	compose := r.FormValue("compose")
	found := false
	for _, c := range folder.Composes {
		found = found || c == compose
	}
	if !found {
		s.discoverRedirect(w, r, "err", "Choose one of the compose files found in that folder.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !appNameRe.MatchString(name) {
		s.discoverRedirect(w, r, "err", "The name must be lowercase letters, digits and dashes.")
		return
	}
	mode := r.FormValue("mode")
	if mode != "existing" && mode != "clone" {
		s.discoverRedirect(w, r, "err", "Choose how to run "+name+": use the existing folder, or clone a new copy.")
		return
	}
	if folder.Remote == "" {
		s.discoverRedirect(w, r, "err", "That folder has no git remote, so Berth cannot clone or update it. Add it by hand with Register app.")
		return
	}
	if mode == "existing" && (!folder.HasGit || folder.Branch == "") {
		s.discoverRedirect(w, r, "err", "Cannot use the existing folder: it is not a git checkout on a branch. Clone a new copy instead.")
		return
	}
	dir := filepath.Dir(compose)
	if dir == "." {
		dir = ""
	}
	app := store.App{Name: name, RepoURL: folder.Remote, Branch: folder.Branch, Subfolder: dir, ComposeFile: filepath.Base(compose),
		SourcePath: folder.Path, SourceMode: mode, ComposeProject: strings.TrimSpace(r.FormValue("project"))}
	if app.Branch == "" {
		app.Branch = "main"
	}
	err = deploy.ValidateApp(app)
	if err == nil {
		err = s.Store.CreateApp(r.Context(), app)
	}
	if err != nil {
		s.discoverRedirect(w, r, "err", "Could not add "+name+": the name is already used, or a setting is invalid (compose project: lowercase letters, digits, - and _).")
		return
	}
	s.Store.Audit(r.Context(), a.Username, "app.create", app.Name+" (added from discovery)")
	s.redirect(w, r, "/apps/"+app.Name+"?msg="+url.QueryEscape("added from discovery"))
}

// ---- Berth's own logs and audit trail ----

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	const pageSize = 100
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := s.Store.ListAudit(r.Context(), pageSize+1, before)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	var next int64
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		next = rows[len(rows)-1].ID
	}
	s.render(w, "audit", map[string]any{"User": a.Username, "Rows": rows, "Next": next})
}

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	s.render(w, "logs", map[string]any{"User": a.Username, "Enabled": s.Logs != nil})
}

// logsStream sends Berth's recent log lines, then new ones as they are written (server-sent events).
func (s *Server) logsStream(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	fl, ok := w.(http.Flusher)
	if s.Logs == nil || !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	recent, live, cancel := s.Logs.Subscribe(300)
	defer cancel()
	for _, l := range recent {
		fmt.Fprintf(w, "data: %s\n\n", l)
	}
	fl.Flush()
	keepAlive := time.NewTicker(25 * time.Second) // keeps proxies from closing an idle stream
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-live:
			fmt.Fprintf(w, "data: %s\n\n", l)
			fl.Flush()
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		}
	}
}
