package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

// This file measures backups received by TusK. It only reads the receiver's "received" folder:
// every night each agent sends a small JSON report of its run into received/reports/, and the files
// on disk are the record (TusK's own database is only a copy of them). Nothing here writes or deletes.

// backupSource and backupRun mirror the JSON the agent writes (internal/report in the TusK repository).
type backupSource struct {
	Name         string `json:"name"`
	Mode         string `json:"mode"`
	Status       string `json:"status"`
	FilesChanged int    `json:"files_changed"`
	BytesSent    int64  `json:"bytes_sent"`
	DurationMS   int64  `json:"duration_ms"`
	Error        string `json:"error"`
}

type backupRun struct {
	Host       string         `json:"host"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Status     string         `json:"status"`
	Note       string         `json:"note"`
	Sources    []backupSource `json:"sources"`
}

// readBackupRuns returns every readable report, newest first, and how many files could not be read.
// A folder without a reports/ subfolder is not an error: no agent has reported yet.
func readBackupRuns(dir string) (runs []backupRun, unreadable int, err error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "reports"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "reports", e.Name()))
		var r backupRun
		if err != nil || json.Unmarshal(b, &r) != nil || r.StartedAt.IsZero() {
			unreadable++
			continue
		}
		if r.Host == "" {
			r.Host = "unknown"
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].StartedAt.After(runs[b].StartedAt) })
	return runs, unreadable, nil
}

type backupStorage struct {
	Name   string
	Files  int
	Bytes  int64
	Newest time.Time
}

// scanBackupStorage totals what is really on disk under dir, per top-level folder (the source name).
func scanBackupStorage(dir string) ([]backupStorage, backupStorage) {
	by := map[string]*backupStorage{}
	var total backupStorage
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(dir, path)
		info, e2 := d.Info()
		if e != nil || e2 != nil {
			return nil
		}
		name := "(root)"
		if i := strings.Index(filepath.ToSlash(rel), "/"); i > 0 {
			name = filepath.ToSlash(rel)[:i]
		}
		s := by[name]
		if s == nil {
			s = &backupStorage{Name: name}
			by[name] = s
		}
		s.Files++
		s.Bytes += info.Size()
		if info.ModTime().After(s.Newest) {
			s.Newest = info.ModTime()
		}
		total.Files++
		total.Bytes += info.Size()
		return nil
	})
	out := make([]backupStorage, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, total
}

func utc(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// failedSources lists the sources of a run that did not succeed, with their error.
func failedSources(r backupRun) string {
	var parts []string
	for _, s := range r.Sources {
		if s.Status != "ok" {
			p := s.Name
			if s.Error != "" {
				p += ": " + s.Error
			}
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

// collectBackups judges the backups per host. A host is stale when it has no successful run within
// "stale_hours" (a run that happened and failed is not reassurance), and degraded when it is not stale
// but its latest run did not succeed for every source.
func collectBackups(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	dir := strings.TrimSpace(sp.Params["dir"])
	if dir == "" {
		return fail(fmt.Errorf("no backups folder is set"))
	}
	runs, unreadable, err := readBackupRuns(dir)
	if err != nil {
		return fail(fmt.Errorf("cannot read %s: %w", dir, err))
	}
	r := newResult()
	r.Up = true
	r.Info["dir"] = dir
	if unreadable > 0 {
		r.Info["unreadable_reports"] = fmt.Sprint(unreadable)
	}
	staleAfter := time.Duration(Threshold(sp.Params, "stale_hours", 26) * float64(time.Hour))

	type hostState struct {
		latest, lastOK *backupRun
	}
	hosts := map[string]*hostState{}
	var names []string
	for i := range runs { // newest first
		h := hosts[runs[i].Host]
		if h == nil {
			h = &hostState{latest: &runs[i]}
			hosts[runs[i].Host] = h
			names = append(names, runs[i].Host)
		}
		if h.lastOK == nil && runs[i].Status == "ok" {
			h.lastOK = &runs[i]
		}
	}
	sort.Strings(names)

	latestT := store.Table{Title: "Latest run per host", Columns: []string{"Host", "Started (UTC)", "Status", "Last good run (UTC)"}}
	sourcesT := store.Table{Title: "Sources in the latest run", Columns: []string{"Host", "Source", "Mode", "Status", "Files changed", "Sent", "Took", "Error"}}
	var stale, degraded []string
	var worst float64
	for _, n := range names {
		h := hosts[n]
		isStale := h.lastOK == nil || now.Sub(h.lastOK.StartedAt) > staleAfter
		switch {
		case h.lastOK == nil:
			stale = append(stale, n+" (no successful run recorded)")
		case isStale:
			stale = append(stale, fmt.Sprintf("%s (%.0f hours since the last good run)", n, now.Sub(h.lastOK.StartedAt).Hours()))
		case h.latest.Status != "ok":
			degraded = append(degraded, n+" ("+h.latest.Status+": "+failedSources(*h.latest)+")")
		}
		if h.lastOK != nil {
			if age := now.Sub(h.lastOK.StartedAt).Hours(); age > worst {
				worst = age
			}
		}
		good := "never"
		if h.lastOK != nil {
			good = utc(h.lastOK.StartedAt)
		}
		latestT.Rows = append(latestT.Rows, []string{n, utc(h.latest.StartedAt), h.latest.Status, good})
		for _, s := range h.latest.Sources {
			sourcesT.Rows = append(sourcesT.Rows, []string{n, s.Name, s.Mode, s.Status, fmt.Sprint(s.FilesChanged), humanBytes(float64(s.BytesSent)),
				(time.Duration(s.DurationMS) * time.Millisecond).Round(time.Second).String(), s.Error})
		}
	}

	recent := store.Table{Title: "Recent runs", Columns: []string{"Host", "Started (UTC)", "Took", "Status", "Problem"}}
	for i, run := range runs {
		if i >= 15 {
			break
		}
		problem := failedSources(run)
		if problem == "" {
			problem = run.Note
		}
		recent.Rows = append(recent.Rows, []string{run.Host, utc(run.StartedAt), run.FinishedAt.Sub(run.StartedAt).Round(time.Second).String(), run.Status, problem})
	}

	perSource, total := scanBackupStorage(dir)
	storage := store.Table{Title: "Storage", Columns: []string{"Folder", "Files", "Size", "Newest file (UTC)"}}
	for _, s := range perSource {
		storage.Rows = append(storage.Rows, []string{s.Name, fmt.Sprint(s.Files), humanBytes(float64(s.Bytes)), utc(s.Newest)})
	}

	r.Metrics["backup_hosts"] = float64(len(names))
	r.Metrics["stale_hosts"] = float64(len(stale))
	r.Metrics["degraded_hosts"] = float64(len(degraded))
	r.Metrics["worst_age_hours"] = worst
	r.Metrics["storage_bytes"] = float64(total.Bytes)
	r.Metrics["storage_files"] = float64(total.Files)
	r.Info["stale"] = strings.Join(stale, "; ")
	r.Info["degraded"] = strings.Join(degraded, "; ")
	for _, t := range []store.Table{latestT, sourcesT, recent, storage} {
		if len(t.Rows) > 0 {
			r.Tables = append(r.Tables, t)
		}
	}
	return r
}
