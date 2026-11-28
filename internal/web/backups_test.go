package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

func TestBackupsPage(t *testing.T) {
	ts, _, srv, cookie := setupLoggedIn(t)
	ctx := context.Background()
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
	if r, _ := c.Get(ts.URL + "/backups"); r.StatusCode != 303 {
		t.Fatalf("the Backups page needs a login, got %d", r.StatusCode)
	}
	if page := get("/backups"); !strings.Contains(page, "BERTH_BACKUPS_DIR") || !strings.Contains(page, "never changes or deletes") {
		t.Fatalf("when backups are not set up the page says how to turn them on")
	}
	if !strings.Contains(get("/"), `href="/backups"`) {
		t.Errorf("no Backups item in the menu")
	}

	id, err := srv.Store.CreateService(ctx, store.Service{Name: "backups", Kind: "backups", Enabled: true, Params: map[string]string{"dir": "/data/received"}})
	if err != nil {
		t.Fatal(err)
	}
	if page := get("/backups"); !strings.Contains(page, "Not checked yet") {
		t.Fatalf("before the first check the page says so")
	}

	save := func(up bool, latest map[string]float64, info map[string]string, errText string) {
		samples := map[string]float64{"up": 1}
		for k, v := range latest {
			samples[k] = v
		}
		tables := []store.Table{
			{Title: "Latest run per host", Columns: []string{"Host", "Started (UTC)", "Status", "Last good run (UTC)"}, Rows: [][]string{{"tams", "2026-11-27 21:32", "partial", "2026-11-26 21:32"}}},
			{Title: "Storage", Columns: []string{"Folder", "Files", "Size", "Newest file (UTC)"}, Rows: [][]string{{"documents", "81", "4.4 GiB", "2026-11-27 21:32"}}},
		}
		if err := srv.Store.SaveCheck(ctx, store.ServiceState{ServiceID: id, Up: up, Error: errText, Latest: latest, Info: info, Tables: tables}, samples, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	info := map[string]string{"dir": "/data/received"}

	save(true, map[string]float64{"backup_hosts": 1, "worst_age_hours": 14.4, "storage_bytes": 4 << 30}, info, "")
	page := get("/backups")
	for _, want := range []string{"Backups are up to date", "tile ok", "oldest good backup is 14.4 hours old", "Latest run per host", `badge warn">partial`, "4.4 GiB", "Storage"} {
		if !strings.Contains(page, want) {
			t.Errorf("healthy page is missing %q", want)
		}
	}

	save(true, map[string]float64{"backup_hosts": 1, "degraded_hosts": 1}, map[string]string{"dir": "/d", "degraded": "tams (partial: documents: connection reset)"}, "")
	if page := get("/backups"); !strings.Contains(page, "tile warn") || !strings.Contains(page, "documents: connection reset") {
		t.Errorf("a partial run is amber and names the failed source")
	}

	save(true, map[string]float64{"backup_hosts": 1, "stale_hosts": 1}, map[string]string{"dir": "/d", "stale": "tams (40 hours since the last good run)"}, "")
	if page := get("/backups"); !strings.Contains(page, "tile bad") || !strings.Contains(page, "A backup is overdue") || !strings.Contains(page, "40 hours since the last good run") {
		t.Errorf("an overdue backup is red and names the host")
	}

	save(true, map[string]float64{"backup_hosts": 0}, info, "")
	if page := get("/backups"); !strings.Contains(page, "No backup reports yet") {
		t.Errorf("no reports is called out")
	}

	save(false, nil, info, "cannot read /data/received: permission denied")
	if page := get("/backups"); !strings.Contains(page, "Cannot read the backups folder") || !strings.Contains(page, "permission denied") {
		t.Errorf("an unreadable folder is red with the reason")
	}

	// the check is not listed among the services, and an open alert shows on the page
	if list := get("/services"); strings.Contains(list, ">backups<") {
		t.Errorf("the Services page lists only the real services")
	}
	srv.Store.OpenAlert(ctx, id, "stale", "critical", "no successful backup within 26 hours")
	if page := get("/backups"); !strings.Contains(page, "Active alerts") || !strings.Contains(page, "no successful backup within 26 hours") {
		t.Errorf("open alerts are shown")
	}
}
