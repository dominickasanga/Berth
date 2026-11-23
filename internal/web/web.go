// Package web serves Berth's UI (html/template + htmx) and enforces admin login.
package web

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dominickasanga/berth/config"
	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
	"github.com/dominickasanga/berth/internal/discover"
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
	for _, page := range []string{"login", "apps", "app_new", "app", "discover"} {
		t, err := template.New("").ParseFS(tplFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		s.tpl[page] = t
	}
	frag, err := template.New("").ParseFS(tplFS, "templates/fragments.html")
	if err != nil {
		return nil, err
	}
	s.tpl["fragments"] = frag

	static, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.auth(s.logout))
	mux.HandleFunc("GET /{$}", s.auth(s.appsPage))
	mux.HandleFunc("GET /discover", s.auth(s.discoverPage))
	mux.HandleFunc("POST /discover/paths", s.auth(s.discoverAddPath))
	mux.HandleFunc("POST /discover/paths/delete", s.auth(s.discoverDeletePath))
	mux.HandleFunc("POST /discover/choose", s.auth(s.discoverChoose))
	mux.HandleFunc("POST /discover/register", s.auth(s.discoverRegister))
	mux.HandleFunc("GET /apps/new", s.auth(s.appNewPage))
	mux.HandleFunc("POST /apps", s.auth(s.appCreate))
	mux.HandleFunc("GET /apps/{name}", s.auth(s.appPage))
	mux.HandleFunc("GET /apps/{name}/status", s.auth(s.appStatus))
	mux.HandleFunc("POST /apps/{name}/deploy", s.auth(s.appDeploy))
	mux.HandleFunc("POST /apps/{name}/control", s.auth(s.appControl))
	mux.HandleFunc("POST /apps/{name}/rollback", s.auth(s.appRollback))
	mux.HandleFunc("GET /apps/{name}/logs/stream", s.auth(s.logStream))
	mux.HandleFunc("POST /apps/{name}/env", s.auth(s.envSet))
	mux.HandleFunc("POST /apps/{name}/env/delete", s.auth(s.envDelete))
	mux.HandleFunc("GET /deploys/{id}/log", s.auth(s.deployLog))
	return s.sameOrigin(secureHeaders(mux)), nil
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
				next(w, r, a)
				return
			}
		}
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", "/login")
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := "layout"
	if page == "fragments" {
		name = ""
	}
	t := s.tpl[page]
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
		s.noteFail(user)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", map[string]string{"Error": "Wrong username, password or code."})
	}
	if s.tooManyFails(user) {
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
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: s.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	s.Store.Audit(r.Context(), a.Username, "login", "")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.Store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	http.Redirect(w, r, "/apps/"+app.Name, http.StatusSeeOther)
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
	s.render(w, "app", map[string]any{"User": a.Username, "App": app, "Env": rows, "Deploys": deploys, "Flash": r.URL.Query().Get("msg")})
}

func (s *Server) appStatus(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	out, err := s.Deploy.Status(*app)
	data := map[string]any{"Out": out}
	if err != nil {
		data["Err"] = err.Error()
	}
	s.render(w, "fragments", frag{"status", data})
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
	http.Redirect(w, r, "/apps/"+app.Name+"?msg=deploy+"+strconv.FormatInt(id, 10)+"+started", http.StatusSeeOther)
}

func (s *Server) appControl(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	action := r.FormValue("action")
	out, err := s.Deploy.Control(*app, action)
	s.Store.Audit(r.Context(), a.Username, "app."+action, app.Name)
	msg := action + " ok"
	if err != nil {
		msg = action + " failed: " + strings.TrimSpace(out)
	}
	http.Redirect(w, r, "/apps/"+app.Name+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
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
	http.Redirect(w, r, "/apps/"+app.Name+"?msg=saved+"+url.QueryEscape(key)+"+(applies+on+next+deploy)", http.StatusSeeOther)
}

func (s *Server) envDelete(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	app := s.loadApp(w, r)
	if app == nil {
		return
	}
	key := r.FormValue("key")
	s.Store.DeleteEnv(r.Context(), app.ID, key)
	s.Store.Audit(r.Context(), a.Username, "env.delete", app.Name+":"+key)
	http.Redirect(w, r, "/apps/"+app.Name, http.StatusSeeOther)
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
	http.Redirect(w, r, "/apps/"+app.Name+"?msg=rollback+"+strconv.FormatInt(id, 10)+"+started", http.StatusSeeOther)
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
		rows = append(rows, row)
	}
	s.render(w, "discover", map[string]any{"User": a.Username, "Paths": paths, "Notes": notes, "Rows": rows,
		"Flash": r.URL.Query().Get("msg"), "Error": r.URL.Query().Get("err")})
}

func discoverRedirect(w http.ResponseWriter, r *http.Request, key, msg string) {
	http.Redirect(w, r, "/discover?"+key+"="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) discoverAddPath(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	p := strings.TrimSpace(r.FormValue("path"))
	if p == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		discoverRedirect(w, r, "err", "Enter an absolute folder path such as /var/www")
		return
	}
	p = filepath.Clean(p)
	if err := s.Store.AddScanPath(r.Context(), p); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "scanpath.add", p)
	discoverRedirect(w, r, "msg", "Added "+p)
}

func (s *Server) discoverDeletePath(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	p := r.FormValue("path")
	s.Store.DeleteScanPath(r.Context(), p)
	s.Store.Audit(r.Context(), a.Username, "scanpath.delete", p)
	discoverRedirect(w, r, "msg", "Removed "+p)
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
			discoverRedirect(w, r, "msg", "Using "+path+" for "+x.Title)
			return
		}
	}
	discoverRedirect(w, r, "err", "That folder was not found in this scan; nothing was changed.")
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
	c, need := discover.Resolve(found[x.Name], choices[x.Name])
	if need {
		discoverRedirect(w, r, "err", x.Title+" was found in more than one place. Flag the copy to use first.")
		return
	}
	app := store.App{Name: x.Name, RepoURL: x.Repo, Branch: x.Branch, Subfolder: x.Subfolder,
		ComposeFile: x.ComposeFile, PredeploySvc: x.PredeployService, Monitor: x.Monitor}
	if c != nil {
		app.SourcePath = c.Path
		if c.RemoteMatch { // the remote already works on this server (e.g. its SSH form)
			app.RepoURL = c.Remote
		}
		if c.RemoteMatch && c.Branch != "" && !strings.HasPrefix(c.Branch, "-") {
			app.Branch = c.Branch
		}
	}
	err = deploy.ValidateApp(app)
	if err == nil {
		err = s.Store.CreateApp(r.Context(), app)
	}
	if err != nil {
		discoverRedirect(w, r, "err", "Could not register "+x.Title+": already registered or invalid settings.")
		return
	}
	s.Store.Audit(r.Context(), a.Username, "app.create", app.Name+" (discovered)")
	http.Redirect(w, r, "/apps/"+app.Name+"?msg="+url.QueryEscape("registered from discovery"), http.StatusSeeOther)
}
