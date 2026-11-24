package web

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dominickasanga/berth/config"
	"github.com/dominickasanga/berth/internal/monitor"
	"github.com/dominickasanga/berth/internal/store"
)

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\-\[\]]{1,253}$`)

type stat struct{ Title, Value string }

type serviceCard struct {
	Service store.Service
	Kind    monitor.KindDef
	State   *store.ServiceState
	Stats   []stat
	Alerts  int
	Muted   bool // alert emails are paused
}

// serviceGroup is one section of the Services page. A service belongs to the group of its kind.
type serviceGroup struct {
	Key, Title, Icon string
	Kinds            []string
	Cards            []serviceCard
	Alerts, Down     int // open alerts, and services that are down, in this group
}

// serviceGroupDefs is the order of the sections. A kind not listed here lands in the last one.
var serviceGroupDefs = []serviceGroup{
	{Key: "databases", Title: "Databases and caches", Icon: "database", Kinds: []string{"postgres", "mysql", "mongo", "redis"}},
	{Key: "messaging", Title: "Messaging", Icon: "message-square", Kinds: []string{"rabbitmq"}},
	{Key: "web", Title: "Apps and APIs", Icon: "globe", Kinds: []string{"http"}},
	{Key: "ports", Title: "Ports and other", Icon: "plug", Kinds: []string{"tcp"}},
}

// groupServices sorts the cards into their sections (keeping their order) and drops empty sections.
func groupServices(cards []serviceCard) []serviceGroup {
	groups := make([]serviceGroup, len(serviceGroupDefs))
	copy(groups, serviceGroupDefs)
	for _, c := range cards {
		i := len(groups) - 1
		for gi, g := range groups {
			for _, k := range g.Kinds {
				if k == c.Service.Kind {
					i = gi
				}
			}
		}
		g := &groups[i]
		g.Cards = append(g.Cards, c)
		g.Alerts += c.Alerts
		if c.Service.Enabled && c.State != nil && !c.State.Up {
			g.Down++
		}
	}
	var out []serviceGroup
	for _, g := range groups {
		if len(g.Cards) > 0 {
			out = append(out, g)
		}
	}
	return out
}

func (s *Server) kindDef(kind string) (monitor.KindDef, bool) { return monitor.Kind(kind) }

// stats formats the metrics a kind shows on its card.
func statsFor(kind monitor.KindDef, st *store.ServiceState) []stat {
	var out []stat
	if st == nil {
		return out
	}
	for _, name := range kind.Card {
		v, ok := st.Latest[name]
		if !ok {
			continue
		}
		title := name
		for _, m := range monitor.MetricsFor(kind.Kind) {
			if m.Name == name {
				title = m.Title
			}
		}
		out = append(out, stat{title, monitor.Format(monitor.MetricUnit(kind.Kind, name), v)})
	}
	return out
}

func (s *Server) servicesPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	ctx := r.Context()
	services, err := s.Store.ListServices(ctx)
	open, err2 := s.Store.OpenAlerts(ctx)
	if err != nil || err2 != nil {
		http.Error(w, "server error", 500)
		return
	}
	perService := map[int64]int{}
	for _, al := range open {
		perService[al.ServiceID]++
	}
	cards := make([]serviceCard, 0, len(services))
	for _, v := range services {
		st, _ := s.Store.ServiceState(ctx, v.ID)
		kd, _ := monitor.Kind(v.Kind)
		cards = append(cards, serviceCard{Muted: v.Muted(time.Now()), Service: v, Kind: kd, State: st, Stats: statsFor(kd, st), Alerts: perService[v.ID]})
	}
	resolved, _ := s.Store.RecentResolvedAlerts(ctx, 8)
	have := map[string]bool{}
	for _, v := range services {
		have[v.Name] = true
	}
	var suggested []config.ExpectedService
	for _, e := range s.ExpectedServices {
		if !have[e.Name] {
			suggested = append(suggested, e)
		}
	}
	s.render(w, "services", map[string]any{"User": a.Username, "Groups": groupServices(cards), "Open": open, "Resolved": resolved, "Suggested": suggested,
		"MailOn": s.MailOn, "Flash": r.URL.Query().Get("msg"), "Error": r.URL.Query().Get("err")})
}

// ---- add / edit ----

type kindView struct {
	Def    monitor.KindDef
	Params map[string]string
}

type serviceForm struct {
	ID               int64
	Name, Kind, Host string
	Port             string
	Username         string
	Params           map[string]string
	Enabled          bool
	HasPassword      bool
	Edit             bool
}

func (s *Server) formPage(w http.ResponseWriter, a *store.Admin, f serviceForm, errMsg string) {
	var kinds []kindView
	for _, k := range monitor.KindNames() {
		d, _ := monitor.Kind(k)
		kinds = append(kinds, kindView{Def: d})
	}
	s.render(w, "service_form", map[string]any{"User": a.Username, "Form": f, "Kinds": kinds, "Error": errMsg})
}

func (s *Server) serviceNewPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	for _, e := range s.ExpectedServices { // "Add" on a suggested service pre-fills the form
		if e.Name == r.URL.Query().Get("preset") {
			params := map[string]string{}
			for k, v := range e.Params {
				params[k] = v
			}
			s.formPage(w, a, serviceForm{Name: e.Name, Kind: e.Kind, Host: e.Host, Port: strconv.Itoa(e.Port), Params: params, Enabled: true}, "")
			return
		}
	}
	kind := r.URL.Query().Get("kind")
	if _, ok := monitor.Kind(kind); !ok {
		kind = "postgres"
	}
	d, _ := monitor.Kind(kind)
	s.formPage(w, a, serviceForm{Kind: kind, Host: "127.0.0.1", Port: strconv.Itoa(d.DefaultPort), Params: map[string]string{}, Enabled: true}, "")
}

// formFromRequest reads and validates the form. The password is returned separately.
func formFromRequest(r *http.Request) (f serviceForm, password string, err string) {
	f = serviceForm{Name: strings.TrimSpace(r.FormValue("name")), Kind: r.FormValue("kind"), Host: strings.TrimSpace(r.FormValue("host")),
		Port: strings.TrimSpace(r.FormValue("port")), Username: strings.TrimSpace(r.FormValue("username")),
		Enabled: r.FormValue("enabled") == "on", Params: map[string]string{}}
	password = r.FormValue("password")
	d, ok := monitor.Kind(f.Kind)
	if !ok {
		return f, password, "Choose a kind of service."
	}
	for _, p := range d.Params {
		if v := strings.TrimSpace(r.FormValue("p_" + p.Key)); v != "" {
			f.Params[p.Key] = v
		}
	}
	for _, t := range d.Thresholds {
		if v := strings.TrimSpace(r.FormValue("t_" + t.Key)); v != "" {
			if _, e := strconv.ParseFloat(v, 64); e != nil {
				return f, password, t.Label + " must be a number."
			}
			f.Params["t_"+t.Key] = v
		}
	}
	if v := strings.TrimSpace(r.FormValue("fail_checks")); v != "" {
		if n, e := strconv.Atoi(v); e != nil || n < 1 || n > 20 {
			return f, password, "Failed checks before 'down' must be a whole number from 1 to 20."
		}
		f.Params["fail_checks"] = v
	}
	if d.NeedsHost {
		if !hostRe.MatchString(f.Host) {
			return f, password, "Enter the host name or IP address (letters, digits, dots and dashes)."
		}
		if n, e := strconv.Atoi(f.Port); e != nil || n < 1 || n > 65535 {
			return f, password, "The port must be a number from 1 to 65535."
		}
	}
	if f.Kind == "http" {
		u, e := url.Parse(f.Params["url"])
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return f, password, "The URL must start with http:// or https://"
		}
		if f.Host == "" {
			f.Host = u.Hostname()
		}
		if f.Port == "" {
			if p := u.Port(); p != "" {
				f.Port = p
			} else if u.Scheme == "https" {
				f.Port = "443"
			} else {
				f.Port = "80"
			}
		}
	}
	if strings.ContainsAny(f.Username, "\r\n\x00") || len(f.Username) > 100 {
		return f, password, "That username is not valid."
	}
	return f, password, ""
}

func (s *Server) serviceCreate(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	f, pw, msg := formFromRequest(r)
	if msg == "" && !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`).MatchString(f.Name) {
		msg = "The name must be lowercase letters, digits and dashes."
	}
	if msg != "" {
		w.WriteHeader(http.StatusBadRequest)
		s.formPage(w, a, f, msg)
		return
	}
	port, _ := strconv.Atoi(f.Port)
	svc := store.Service{Name: f.Name, Kind: f.Kind, Host: f.Host, Port: port, Username: f.Username, Params: f.Params, Enabled: f.Enabled}
	if pw != "" {
		sealed, err := s.Box.Seal(pw)
		if err != nil {
			http.Error(w, "server error", 500)
			return
		}
		svc.Password = sealed
	}
	id, err := s.Store.CreateService(r.Context(), svc)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.formPage(w, a, f, "Could not save: that name is already used.")
		return
	}
	s.Store.Audit(r.Context(), a.Username, "service.create", f.Name)
	if s.Sampler != nil && svc.Enabled {
		svc.ID = id
		go s.Sampler.CheckService(context.Background(), svc) // the first check, so the page has data
	}
	s.redirect(w, r, "/services/"+strconv.FormatInt(id, 10)+"?msg="+url.QueryEscape("added; the first check is running"))
}

// savedPasswordStale says the saved password cannot be reused: it belongs to a different
// username, host or port than the ones now in the form.
func savedPasswordStale(saved *store.Service, f serviceForm) bool {
	port, _ := strconv.Atoi(f.Port)
	return saved.Username != f.Username || saved.Host != f.Host || saved.Port != port
}

const stalePasswordMsg = "You changed the username, host or port, so type the password again: the saved password belongs to the old settings."

func (s *Server) loadService(w http.ResponseWriter, r *http.Request) *store.Service {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	v, err := s.Store.ServiceByID(r.Context(), id)
	if err != nil {
		http.Error(w, "server error", 500)
		return nil
	}
	if v == nil {
		http.NotFound(w, r)
	}
	return v
}

func (s *Server) serviceEditPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	v := s.loadService(w, r)
	if v == nil {
		return
	}
	s.formPage(w, a, serviceForm{ID: v.ID, Name: v.Name, Kind: v.Kind, Host: v.Host, Port: strconv.Itoa(v.Port), Username: v.Username,
		Params: v.Params, Enabled: v.Enabled, HasPassword: len(v.Password) > 0, Edit: true}, "")
}

func (s *Server) serviceUpdate(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	v := s.loadService(w, r)
	if v == nil {
		return
	}
	r.Form = nil
	r.PostForm = nil
	f, pw, msg := formFromRequest(r)
	f.ID, f.Name, f.Kind, f.Edit, f.HasPassword = v.ID, v.Name, v.Kind, true, len(v.Password) > 0
	if r.FormValue("kind") != v.Kind {
		msg = "The kind of a service cannot be changed; add a new one."
	}
	if msg != "" {
		w.WriteHeader(http.StatusBadRequest)
		s.formPage(w, a, f, msg)
		return
	}
	if pw == "" && r.FormValue("clear_password") != "on" && len(v.Password) > 0 && savedPasswordStale(v, f) {
		w.WriteHeader(http.StatusBadRequest)
		s.formPage(w, a, f, stalePasswordMsg)
		return
	}
	port, _ := strconv.Atoi(f.Port)
	upd := store.Service{ID: v.ID, Host: f.Host, Port: port, Username: f.Username, Params: f.Params, Enabled: f.Enabled}
	switch {
	case r.FormValue("clear_password") == "on":
		upd.Password = []byte{} // an empty value clears it
	case pw != "":
		sealed, err := s.Box.Seal(pw)
		if err != nil {
			http.Error(w, "server error", 500)
			return
		}
		upd.Password = sealed
	}
	if err := s.Store.UpdateService(r.Context(), upd); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "service.update", v.Name)
	s.redirect(w, r, "/services/"+strconv.FormatInt(v.ID, 10)+"?msg="+url.QueryEscape("saved"))
}

func (s *Server) serviceDelete(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	v := s.loadService(w, r)
	if v == nil {
		return
	}
	s.Store.DeleteService(r.Context(), v.ID)
	s.Store.Audit(r.Context(), a.Username, "service.delete", v.Name)
	s.redirect(w, r, "/services?msg="+url.QueryEscape("removed "+v.Name+" and its history"))
}

func (s *Server) serviceCheck(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	v := s.loadService(w, r)
	if v == nil {
		return
	}
	if s.Sampler != nil {
		s.Sampler.CheckService(r.Context(), *v)
	}
	s.redirect(w, r, "/services/"+strconv.FormatInt(v.ID, 10))
}

// muteChoices are the pause lengths offered on the service page; "off" resumes alerts.
var muteOptions = []struct{ Key, Label string }{{"1h", "Pause 1 hour"}, {"4h", "Pause 4 hours"}, {"24h", "Pause 24 hours"}, {"7d", "Pause 7 days"}, {"forever", "Pause until resumed"}}

var muteChoices = map[string]time.Duration{"1h": time.Hour, "4h": 4 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

// serviceMute pauses alert emails for one service for a while, or until resumed, or resumes them.
func (s *Server) serviceMute(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	v := s.loadService(w, r)
	if v == nil {
		return
	}
	choice := r.FormValue("for")
	var until *time.Time
	switch d, ok := muteChoices[choice]; {
	case ok:
		t := time.Now().Add(d)
		until = &t
	case choice == "forever":
		t := store.MutedForever
		until = &t
	case choice == "off":
	default:
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.Store.SetServiceMute(r.Context(), v.ID, until); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	msg := "alert emails resumed"
	if until != nil {
		msg = "alert emails paused"
	}
	s.Store.Audit(r.Context(), a.Username, "service.mute", v.Name+" "+choice)
	s.redirect(w, r, "/services/"+strconv.FormatInt(v.ID, 10)+"?msg="+url.QueryEscape(msg))
}

// serviceTest tries the form's settings once without saving them (an htmx fragment).
func (s *Server) serviceTest(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	f, pw, msg := formFromRequest(r)
	if msg != "" {
		s.render(w, "fragments", frag{"svc-test", map[string]any{"Problem": msg}})
		return
	}
	if id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64); id > 0 && pw == "" { // editing: use the saved password
		if saved, _ := s.Store.ServiceByID(r.Context(), id); saved != nil && len(saved.Password) > 0 {
			if savedPasswordStale(saved, f) {
				s.render(w, "fragments", frag{"svc-test", map[string]any{"Problem": stalePasswordMsg}})
				return
			}
			if plain, err := s.Box.Open(saved.Password); err == nil {
				pw = plain
			}
		}
	}
	port, _ := strconv.Atoi(f.Port)
	sp := monitor.Spec{Name: f.Name, Kind: f.Kind, Host: f.Host, Port: port, Username: f.Username, Password: pw, Params: f.Params}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res := monitor.Collector(f.Kind)(ctx, sp, monitor.Prev{}, time.Now())
	var stats []stat
	for _, m := range monitor.MetricsFor(f.Kind) {
		if v, ok := res.Metrics[m.Name]; ok {
			stats = append(stats, stat{m.Title, monitor.Format(m.Unit, v)})
		}
	}
	info := infoRows(res.Info)
	s.Store.Audit(r.Context(), a.Username, "service.test", f.Kind+" "+f.Host+":"+f.Port)
	s.render(w, "fragments", frag{"svc-test", map[string]any{"Up": res.Up, "Err": res.Err, "Info": info, "Stats": stats}})
}

func infoRows(m map[string]string) []stat {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]stat, 0, len(keys))
	for _, k := range keys {
		out = append(out, stat{strings.ReplaceAll(k, "_", " "), m[k]})
	}
	return out
}

// ---- detail with charts ----

type chartView struct {
	Title, Last string
	SVG         template.HTML
}

var ranges = []struct {
	Key   string
	Label string
	Span  time.Duration
}{{"1h", "1 hour", time.Hour}, {"24h", "24 hours", 24 * time.Hour}, {"7d", "7 days", 7 * 24 * time.Hour}}

func (s *Server) serviceDetail(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	v := s.loadService(w, r)
	if v == nil {
		return
	}
	ctx := r.Context()
	rangeKey := r.URL.Query().Get("range")
	span := 24 * time.Hour
	found := false
	for _, rg := range ranges {
		if rg.Key == rangeKey {
			span, found = rg.Span, true
		}
	}
	if !found {
		rangeKey = "24h"
	}
	now := time.Now()
	from := now.Add(-span)
	bucket := span / 150
	if bucket < 30*time.Second {
		bucket = 30 * time.Second
	}
	kd, _ := monitor.Kind(v.Kind)
	st, _ := s.Store.ServiceState(ctx, v.ID)
	var charts []chartView
	for _, m := range monitor.MetricsFor(v.Kind) {
		pts, err := s.Store.Series(ctx, v.ID, m.Name, from, int(bucket.Seconds()))
		if err != nil || (len(pts) == 0 && m.Name != "up") {
			continue
		}
		last := ""
		if len(pts) > 0 {
			last = monitor.Format(m.Unit, pts[len(pts)-1].V)
		}
		charts = append(charts, chartView{Title: m.Title, Last: last, SVG: chartSVG(m.Title, pts, m.Unit, from, now, bucket)})
	}
	open, _ := s.Store.OpenAlertsFor(ctx, v.ID)
	var info []stat
	var tables []store.Table
	if st != nil {
		info, tables = infoRows(st.Info), st.Tables
	}
	s.render(w, "service", map[string]any{"User": a.Username, "S": v, "Kind": kd, "State": st, "Info": info, "Tables": tables, "Charts": charts,
		"Open": open, "Range": rangeKey, "Ranges": ranges, "Cards": statsFor(kd, st), "HasPassword": len(v.Password) > 0,
		"Flash": r.URL.Query().Get("msg"), "MailOn": s.MailOn, "Muted": v.Muted(now), "MutedForever": v.MutedIndefinitely(), "MuteOptions": muteOptions})
}
