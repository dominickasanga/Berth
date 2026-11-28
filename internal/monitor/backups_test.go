package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

var backupNow = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

func writeRun(t *testing.T, dir, host string, started time.Time, status string, sources ...backupSource) {
	t.Helper()
	rd := filepath.Join(dir, "reports")
	if err := os.MkdirAll(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(backupRun{Host: host, StartedAt: started, FinishedAt: started.Add(3 * time.Second), Status: status, Sources: sources})
	name := fmt.Sprintf("%s-%s.json", host, started.Format("2006-01-02_150405"))
	if err := os.WriteFile(filepath.Join(rd, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collectBackupsAt(dir string, params map[string]string) Result {
	p := map[string]string{"dir": dir}
	for k, v := range params {
		p[k] = v
	}
	return collectBackups(context.Background(), Spec{Name: "backups", Kind: "backups", Params: p}, Prev{}, backupNow)
}

func condKeys(cs []Condition) string {
	var k []string
	for _, c := range cs {
		k = append(k, c.Key)
	}
	return strings.Join(k, ",")
}

var okSrc = backupSource{Name: "backups", Mode: "latest", Status: "ok"}
var badSrc = backupSource{Name: "documents", Mode: "incremental", Status: "failed", Error: "connection reset"}

// The exact bytes tusk-agent writes (generated with the TusK repository's report package).
func TestBackupsReadsTheAgentsOwnReportFormat(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "reports"), 0o755)
	real := `{"host":"tams","started_at":"2026-11-27T21:32:08Z","finished_at":"2026-11-27T21:32:11Z","status":"partial","sources":[{"name":"backups","mode":"latest","status":"ok","files_changed":1,"files_tracked":72,"bytes_sent":5608795,"archive":"2026-11-27_21-00-00_UTC.dump.tar.gz","attempts":1,"duration_ms":2100},{"name":"documents","mode":"incremental","status":"failed","files_changed":40,"files_tracked":900,"bytes_sent":0,"attempts":3,"duration_ms":800,"error":"connection reset"}]}`
	os.WriteFile(filepath.Join(dir, "reports", "tams-2026-11-27_213208.json"), []byte(real), 0o644)
	// an earlier good run, so the latest partial run is "degraded" and not "stale"
	writeRun(t, dir, "tams", backupNow.Add(-20*time.Hour), "ok", okSrc) // 2026-11-27 16:00, before the partial run

	r := collectBackupsAt(dir, nil)
	if !r.Up || r.Metrics["backup_hosts"] != 1 || r.Metrics["stale_hosts"] != 0 || r.Metrics["degraded_hosts"] != 1 {
		t.Fatalf("one host, not stale, degraded by its latest partial run: %+v %+v", r.Metrics, r.Info)
	}
	if !strings.Contains(r.Info["degraded"], "tams (partial: documents: connection reset)") {
		t.Fatalf("the failed source and its error should be named: %q", r.Info["degraded"])
	}
	if k := condKeys(Conditions("backups", nil, r, 0)); k != "degraded" {
		t.Fatalf("only the degraded warning should open: %q", k)
	}
	titles := map[string]bool{}
	for _, tb := range r.Tables {
		titles[tb.Title] = true
	}
	for _, want := range []string{"Latest run per host", "Sources in the latest run", "Recent runs", "Storage"} {
		if !titles[want] {
			t.Errorf("missing table %q", want)
		}
	}
}

func TestBackupsStaleAndDegraded(t *testing.T) {
	h := func(n float64) time.Time { return backupNow.Add(-time.Duration(n * float64(time.Hour))) }
	cases := []struct {
		name                    string
		runs                    func(dir string)
		params                  map[string]string
		wantStale, wantDegraded float64
		wantKeys                string
	}{
		{"fresh and ok", func(d string) { writeRun(t, d, "tams", h(14), "ok", okSrc) }, nil, 0, 0, ""},
		{"no good run for 30h is stale, even if a later run failed",
			func(d string) {
				writeRun(t, d, "tams", h(30), "ok", okSrc)
				writeRun(t, d, "tams", h(2), "failed", badSrc)
			}, nil, 1, 0, "stale"},
		{"recent good run but the latest run is partial is degraded, not stale",
			func(d string) {
				writeRun(t, d, "tams", h(25), "ok", okSrc)
				writeRun(t, d, "tams", h(1), "partial", okSrc, badSrc)
			}, nil, 0, 1, "degraded"},
		{"a host that never succeeded is stale",
			func(d string) { writeRun(t, d, "tams", h(1), "failed", badSrc) }, nil, 1, 0, "stale"},
		{"one stale host among two is named, the other is fine",
			func(d string) {
				writeRun(t, d, "tams", h(3), "ok", okSrc)
				writeRun(t, d, "silent", h(60), "ok", okSrc)
			}, nil, 1, 0, "stale"},
		{"the stale window can be changed", func(d string) { writeRun(t, d, "tams", h(30), "ok", okSrc) }, map[string]string{"t_stale_hours": "48"}, 0, 0, ""},
		{"no reports at all", func(d string) { os.MkdirAll(d, 0o755) }, nil, 0, 0, "nodata"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.runs(dir)
			r := collectBackupsAt(dir, c.params)
			if !r.Up {
				t.Fatalf("should be up: %s", r.Err)
			}
			if r.Metrics["stale_hosts"] != c.wantStale || r.Metrics["degraded_hosts"] != c.wantDegraded {
				t.Fatalf("stale=%v degraded=%v, want %v and %v (%+v)", r.Metrics["stale_hosts"], r.Metrics["degraded_hosts"], c.wantStale, c.wantDegraded, r.Info)
			}
			params := map[string]string{}
			for k, v := range c.params {
				params[k] = v
			}
			if got := condKeys(Conditions("backups", params, r, 0)); got != c.wantKeys {
				t.Fatalf("conditions %q, want %q", got, c.wantKeys)
			}
		})
	}

	// the message names the host that is behind, not the one that is fine
	dir := t.TempDir()
	writeRun(t, dir, "tams", h(3), "ok", okSrc)
	writeRun(t, dir, "silent", h(60), "ok", okSrc)
	cs := Conditions("backups", nil, collectBackupsAt(dir, nil), 0)
	if len(cs) != 1 || cs[0].Severity != "critical" || !strings.Contains(cs[0].Message, "silent (60 hours since the last good run)") || strings.Contains(cs[0].Message, "tams") {
		t.Fatalf("unexpected message: %+v", cs)
	}
}

func TestBackupsFolderProblems(t *testing.T) {
	// a folder that does not exist is down (after two checks, like any service), not "no data"
	r := collectBackupsAt(filepath.Join(t.TempDir(), "missing"), nil)
	if r.Up || !strings.Contains(r.Err, "cannot read") {
		t.Fatalf("a missing folder should fail the check: %+v", r)
	}
	if len(Conditions("backups", nil, r, 1)) != 0 || condKeys(Conditions("backups", nil, r, 2)) != "down" {
		t.Fatalf("down only after two failed checks")
	}
	if r := collectBackups(context.Background(), Spec{Params: map[string]string{}}, Prev{}, backupNow); r.Up {
		t.Fatal("no folder configured must not be reported as healthy")
	}

	// a broken or empty report is skipped and counted, and does not hide the good ones
	dir := t.TempDir()
	writeRun(t, dir, "tams", backupNow.Add(-time.Hour), "ok", okSrc)
	os.WriteFile(filepath.Join(dir, "reports", "half-written.json"), []byte(`{"host":"x","started_`), 0o644)
	os.WriteFile(filepath.Join(dir, "reports", "notes.txt"), []byte("not a report"), 0o644)
	r = collectBackupsAt(dir, nil)
	if !r.Up || r.Metrics["backup_hosts"] != 1 || r.Info["unreadable_reports"] != "1" {
		t.Fatalf("one good host and one unreadable file: %+v %+v", r.Metrics, r.Info)
	}
}

func TestBackupsStorageIsReadFromDisk(t *testing.T) {
	dir := t.TempDir()
	writeRun(t, dir, "tams", backupNow.Add(-time.Hour), "ok", okSrc)
	writeFile(t, filepath.Join(dir, "backups", "a.dump.tar.gz"), 1000)
	writeFile(t, filepath.Join(dir, "backups", "b.dump.tar.gz"), 500)
	writeFile(t, filepath.Join(dir, "documents", "d1.tar.gz"), 4000)
	before, _ := os.ReadDir(filepath.Join(dir, "reports"))

	r := collectBackupsAt(dir, nil)
	var storage store.Table
	for _, tb := range r.Tables {
		if tb.Title == "Storage" {
			storage = tb
		}
	}
	got := map[string]string{}
	for _, row := range storage.Rows {
		got[row[0]] = row[1] + " " + row[2]
	}
	if got["backups"] != "2 1.5 KiB" || got["documents"] != "1 3.9 KiB" || got["reports"] == "" {
		t.Fatalf("storage per folder: %v", got)
	}
	if r.Metrics["storage_files"] != 4 { // 2 dumps + 1 archive + 1 report
		t.Fatalf("files: %v", r.Metrics["storage_files"])
	}
	// read-only: the folder is exactly as it was
	after, _ := os.ReadDir(filepath.Join(dir, "reports"))
	if len(before) != len(after) {
		t.Fatal("the collector must not change the folder")
	}
}

func TestBackupsServiceFollowsTheSetting(t *testing.T) {
	s, mail, ctx := testSampler(t)
	dir := t.TempDir()
	count := func() (n int, dirParam string) {
		list, _ := s.Store.ListServices(ctx)
		for _, v := range list {
			if v.Kind == "backups" {
				n++
				dirParam = v.Params["dir"]
			}
		}
		return
	}
	// only the backups check matters here: on a Linux machine the server check may raise its own alerts
	openBackups := func() []store.Alert {
		var out []store.Alert
		all, _ := s.Store.OpenAlerts(ctx)
		for _, a := range all {
			if a.ServiceName == "backups" {
				out = append(out, a)
			}
		}
		return out
	}
	mailed := func() []string {
		var out []string
		for _, sub := range mail.subjects() {
			if strings.Contains(sub, ": backups (") {
				out = append(out, sub)
			}
		}
		return out
	}
	s.SyncInternal(ctx)
	if n, _ := count(); n != 0 {
		t.Fatal("backups are not measured unless BERTH_BACKUPS_DIR is set")
	}
	s.BackupsDir = dir
	s.SyncInternal(ctx)
	s.SyncInternal(ctx) // again changes nothing
	if n, p := count(); n != 1 || p != dir {
		t.Fatalf("one backups check on the folder: %d %q", n, p)
	}

	// a night with no good backup opens one critical alert, mailed once
	writeRun(t, dir, "tams", time.Now().Add(-40*time.Hour), "ok", okSrc)
	s.CheckAll(ctx)
	s.CheckAll(ctx)
	open := openBackups()
	if len(open) != 1 || open[0].Key != "stale" || open[0].Severity != "critical" {
		t.Fatalf("a stale backup should open one critical alert: %+v", open)
	}
	if got := mailed(); len(got) != 1 || !strings.Contains(got[0], "FIRING: backups (stale)") {
		t.Fatalf("mailed once: %v", got)
	}

	// the next good run resolves it, and that is mailed too
	writeRun(t, dir, "tams", time.Now().Add(-time.Minute), "ok", okSrc)
	s.CheckAll(ctx)
	if open := openBackups(); len(open) != 0 {
		t.Fatalf("a good run should resolve the alert: %+v", open)
	}
	if got := mailed(); len(got) != 2 || !strings.Contains(got[1], "RESOLVED: backups (stale)") {
		t.Fatalf("resolved mail: %v", got)
	}

	// changing the folder is followed, switching it off removes the check
	other := t.TempDir()
	s.BackupsDir = other
	s.SyncInternal(ctx)
	if _, p := count(); p != other {
		t.Fatalf("the new folder should be followed: %q", p)
	}
	s.BackupsDir = ""
	s.SyncInternal(ctx)
	if n, _ := count(); n != 0 {
		t.Fatal("unsetting the folder removes the check")
	}
}
