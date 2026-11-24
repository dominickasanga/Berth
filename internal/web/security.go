package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/dominickasanga/berth/internal/security"
	"github.com/dominickasanga/berth/internal/store"
)

// secApp is one app's row on the Security page.
type secApp struct {
	Name    string
	Scanned bool
	Scan    security.AppScan
	Vulns   int // vulnerable packages
	Issues  int // container issues
	High    int // of which serious
	Tracked int // secret-like files in the repository
	History int // secret-like files that were removed but are still in the git history
}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	ctx := r.Context()
	apps, err := s.Store.ListApps(ctx)
	scans, err2 := s.Store.SecurityScans(ctx)
	if err != nil || err2 != nil {
		http.Error(w, "server error", 500)
		return
	}
	var rows []secApp
	var last time.Time
	var totVulns, totIssues, totHigh, totTracked, totHistory int
	for _, ap := range apps {
		row := secApp{Name: ap.Name}
		if sc, ok := scans[ap.Name]; ok && json.Unmarshal(sc.Result, &row.Scan) == nil {
			row.Scanned = true
			row.Vulns = row.Scan.VulnerableModules()
			row.Issues = len(row.Scan.Containers)
			for _, c := range row.Scan.Containers {
				if c.Severity == "high" {
					row.High++
				}
			}
			for _, f := range row.Scan.Secrets {
				if f.Tracked {
					row.Tracked++
				} else {
					row.History++
				}
			}
			if sc.At.After(last) {
				last = sc.At
			}
		}
		totVulns, totIssues, totHigh, totTracked, totHistory = totVulns+row.Vulns, totIssues+row.Issues, totHigh+row.High, totTracked+row.Tracked, totHistory+row.History
		rows = append(rows, row)
	}
	// the apps with the most to look at first
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Vulns+rows[i].Issues+rows[i].Tracked+rows[i].History > rows[j].Vulns+rows[j].Issues+rows[j].Tracked+rows[j].History
	})
	scanning := s.Security != nil && s.Security.Scanning()
	var ssh *security.SSHReport
	if s.Access != nil {
		ssh = s.Access.Report()
	}
	day, _ := s.Store.LoginFailures(ctx, time.Now().Add(-24*time.Hour))
	week, _ := s.Store.LoginFailures(ctx, time.Now().Add(-7*24*time.Hour))
	s.render(w, "security", map[string]any{"User": a.Username, "SSH": ssh, "LoginDay": day, "LoginWeek": week, "Apps": rows, "Scanning": scanning, "Enabled": s.Security != nil,
		"Last": last, "Vulns": totVulns, "Issues": totIssues, "High": totHigh, "Tracked": totTracked, "History": totHistory,
		"Flash": r.URL.Query().Get("msg")})
}

func (s *Server) securityScan(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	if s.Security == nil {
		http.Error(w, "not available", 404)
		return
	}
	s.Store.Audit(r.Context(), a.Username, "security.scan", "")
	if s.Security.Scanning() {
		s.redirect(w, r, "/security?msg="+url.QueryEscape("a scan is already running"))
		return
	}
	go s.Security.ScanAll(context.Background()) // outlives this request
	s.redirect(w, r, "/security?msg="+url.QueryEscape("scan started: it takes a minute or two, reload to see the results"))
}

// clientAddress is the visitor's address as far as it can be told: the header Cloudflare sets, else the
// first forwarded address, else the connection's. Behind a proxy the first two are the real ones; if Berth
// were reachable directly, a visitor could write anything there, which is why this is only used to
// show who tried, never to decide who may come in.
func clientAddress(r *http.Request) string {
	for _, h := range []string{"CF-Connecting-IP", "X-Forwarded-For"} {
		if v := r.Header.Get(h); v != "" {
			return oneToken(v)
		}
	}
	return clientHost(r.RemoteAddr)
}

// auditLoginFailure records a failed or refused sign-in with the visitor's address. At most 20 a minute
// are recorded, so an attack cannot flood the audit trail; the log file still has every one.
func (s *Server) auditLoginFailure(r *http.Request, user, action string) {
	s.mu.Lock()
	if time.Since(s.auditFailAt) > time.Minute {
		s.auditFailAt, s.auditFailN = time.Now(), 0
	}
	s.auditFailN++
	allowed := s.auditFailN <= 20
	s.mu.Unlock()
	if !allowed {
		return
	}
	if rs := []rune(user); len(rs) > 64 {
		user = string(rs[:64])
	}
	s.Store.Audit(r.Context(), user, action, clientAddress(r))
}
