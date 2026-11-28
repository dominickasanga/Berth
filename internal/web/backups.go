package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

type backupBanner struct {
	Level, Title, Detail string // Level: ok, warn, bad, mute
}

// tableCell is one cell of a table; a status gets a coloured badge.
type tableCell struct {
	Text, Badge string
}

type tableView struct {
	Title   string
	Columns []string
	Rows    [][]tableCell
}

func statusBadge(text string) string {
	switch text {
	case "ok":
		return "ok"
	case "partial":
		return "warn"
	case "failed":
		return "bad"
	}
	return ""
}

func tableViews(tables []store.Table) []tableView {
	out := make([]tableView, 0, len(tables))
	for _, t := range tables {
		v := tableView{Title: t.Title, Columns: t.Columns}
		for _, row := range t.Rows {
			cells := make([]tableCell, len(row))
			for i, c := range row {
				cells[i].Text = c
				if i < len(t.Columns) && t.Columns[i] == "Status" {
					cells[i].Badge = statusBadge(c)
				}
			}
			v.Rows = append(v.Rows, cells)
		}
		out = append(out, v)
	}
	return out
}

// backupBannerFor answers "are the backups fine?" first, the way TusK's own dashboard does: red when no
// successful run is recent enough, amber when the latest run did not complete every source.
func backupBannerFor(st *store.ServiceState) backupBanner {
	switch {
	case st == nil:
		return backupBanner{"mute", "Not checked yet", "The first check runs within 30 seconds of starting Berth."}
	case !st.Up:
		return backupBanner{"bad", "Cannot read the backups folder", st.Error}
	case st.Latest["backup_hosts"] < 1:
		return backupBanner{"warn", "No backup reports yet", "No agent has reported into " + st.Info["dir"] + "/reports, or the folder is wrong."}
	case st.Latest["stale_hosts"] >= 1:
		return backupBanner{"bad", "A backup is overdue", st.Info["stale"]}
	case st.Latest["degraded_hosts"] >= 1:
		return backupBanner{"warn", "The latest run did not finish every source", st.Info["degraded"]}
	}
	return backupBanner{"ok", "Backups are up to date", fmt.Sprintf("%.0f host(s) reporting; the oldest good backup is %.1f hours old.", st.Latest["backup_hosts"], st.Latest["worst_age_hours"])}
}

func (s *Server) backupsPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	ctx := r.Context()
	all, err := s.Store.ListServices(ctx)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	data := map[string]any{"User": a.Username}
	for _, v := range all {
		if v.Kind != "backups" {
			continue
		}
		rangeKey, span := r.URL.Query().Get("range"), 24*time.Hour
		found := false
		for _, rg := range ranges {
			if rg.Key == rangeKey {
				span, found = rg.Span, true
			}
		}
		if !found {
			rangeKey = "24h"
		}
		st, _ := s.Store.ServiceState(ctx, v.ID)
		open, _ := s.Store.OpenAlertsFor(ctx, v.ID)
		data["Enabled"] = true
		data["ServiceID"] = v.ID
		data["Banner"] = backupBannerFor(st)
		data["Open"] = open
		data["Ranges"], data["Range"] = ranges, rangeKey
		data["Charts"] = s.seriesCharts(ctx, v.ID, "backups", []string{"worst_age_hours", "storage_bytes"}, span)
		if st != nil {
			data["Tables"] = tableViews(st.Tables)
			data["Unreadable"] = st.Info["unreadable_reports"]
		}
		break
	}
	s.render(w, "backups", data)
}
