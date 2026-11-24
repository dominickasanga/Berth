package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/dominickasanga/berth/internal/monitor"
	"github.com/dominickasanga/berth/internal/store"
)

// tile is one number of the "this server" strip. Level (ok, warn, bad) colours its bar.
type tile struct {
	Title, Icon, Value, Sub, Level string
	Pct                            float64
	HasPct                         bool
}

// appRow is one app on the Overview: what its containers are doing right now.
type appRow struct {
	Name     string
	Status   string // "2/2 running", "stopped", "not deployed"
	Level    string
	CPU, Mem string
	Restarts string
	Alerts   int
	HasData  bool
}

type groupSummary struct {
	Key, Title, Icon    string
	Total, Down, Alerts int
}

func levelFor(pct float64) string {
	switch {
	case pct >= 90:
		return "bad"
	case pct >= 75:
		return "warn"
	}
	return "ok"
}

// systemTiles turns the server's latest check into the numbers at the top of the Overview.
func systemTiles(st *store.ServiceState) []tile {
	if st == nil || len(st.Latest) == 0 {
		return nil
	}
	m, info := st.Latest, st.Info
	var out []tile
	if v, ok := m["cpu_pct"]; ok {
		sub := ""
		if l, ok := m["load1"]; ok {
			sub = fmt.Sprintf("load %.2f", l)
			if n := info["cpus"]; n != "" {
				sub += " on " + n + " CPUs"
			}
		}
		out = append(out, tile{Title: "CPU", Icon: "cpu", Value: monitor.Format("pct", v), Sub: sub, Pct: v, HasPct: true, Level: levelFor(v)})
	}
	if v, ok := m["mem_pct"]; ok {
		out = append(out, tile{Title: "Memory", Icon: "memory-stick", Value: monitor.Format("pct", v),
			Sub: monitor.Format("bytes", m["mem_used_bytes"]) + " of " + info["memory"], Pct: v, HasPct: true, Level: levelFor(v)})
	}
	if v, ok := m["disk_pct"]; ok {
		out = append(out, tile{Title: "Disk", Icon: "hard-drive", Value: monitor.Format("pct", v), Sub: "fullest: " + info["disk_worst"], Pct: v, HasPct: true, Level: levelFor(v)})
	}
	if rx, ok := m["net_rx_bytes_s"]; ok {
		out = append(out, tile{Title: "Network", Icon: "network", Value: "in " + monitor.Format("bytes_s", rx),
			Sub: "out " + monitor.Format("bytes_s", m["net_tx_bytes_s"]), Level: "ok"})
	}
	return out
}

// buildCards loads the latest state of each service for the cards of the Services page and the Overview.
func (s *Server) buildCards(r *http.Request, services []store.Service, open []store.Alert) []serviceCard {
	perService := map[int64]int{}
	for _, al := range open {
		perService[al.ServiceID]++
	}
	cards := make([]serviceCard, 0, len(services))
	for _, v := range services {
		st, _ := s.Store.ServiceState(r.Context(), v.ID)
		kd, _ := monitor.Kind(v.Kind)
		cards = append(cards, serviceCard{Muted: v.Muted(time.Now()), Service: v, Kind: kd, State: st, Stats: statsFor(kd, st), Alerts: perService[v.ID]})
	}
	return cards
}

func (s *Server) overviewPage(w http.ResponseWriter, r *http.Request, a *store.Admin) {
	ctx := r.Context()
	all, err := s.Store.ListServices(ctx)
	open, err2 := s.Store.OpenAlerts(ctx)
	apps, err3 := s.Store.ListApps(ctx)
	if err != nil || err2 != nil || err3 != nil {
		http.Error(w, "server error", 500)
		return
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

	byName := map[string]store.Service{}
	perService := map[int64]int{}
	for _, v := range all {
		byName[v.Name] = v
	}
	for _, al := range open {
		perService[al.ServiceID]++
	}

	data := map[string]any{"User": a.Username, "Open": open, "Ranges": ranges, "Range": rangeKey}
	if srv, ok := byName["server"]; ok {
		st, _ := s.Store.ServiceState(ctx, srv.ID)
		data["Tiles"] = systemTiles(st)
		data["Charts"] = s.seriesCharts(ctx, srv.ID, "system", []string{"cpu_pct", "mem_pct", "disk_pct", "net_rx_bytes_s", "net_tx_bytes_s"}, span)
		if st != nil {
			data["Uptime"] = st.Info["uptime"]
			if len(st.Tables) > 0 {
				data["Disks"] = st.Tables[0]
			}
		}
		data["ServerID"] = srv.ID
	}

	var rows []appRow
	for _, ap := range apps {
		row := appRow{Name: ap.Name, Status: "not measured", Level: "mute"}
		if v, ok := byName[monitor.InternalName(ap.Name)]; ok {
			row.Alerts = perService[v.ID]
			if st, _ := s.Store.ServiceState(ctx, v.ID); st != nil && st.Up {
				m := st.Latest
				run, total := int(m["containers_running"]), int(m["containers_total"])
				row.HasData = true
				switch {
				case total == 0:
					row.Status, row.Level = "not deployed", "mute"
				case run == 0:
					row.Status, row.Level = "stopped", "warn"
				default:
					row.Status, row.Level = fmt.Sprintf("%d/%d running", run, total), "ok"
					if run < total {
						row.Level = "warn"
					}
					row.CPU, row.Mem = monitor.Format("pct_core", m["cpu_pct"]), monitor.Format("bytes", m["mem_bytes"])
					row.Restarts = fmt.Sprintf("%.0f", m["restarts"])
				}
			}
		}
		rows = append(rows, row)
	}
	data["Apps"] = rows

	var sums []groupSummary
	for _, g := range groupServices(s.buildCards(r, visibleServices(all), open)) {
		sums = append(sums, groupSummary{Key: g.Key, Title: g.Title, Icon: g.Icon, Total: len(g.Cards), Down: g.Down, Alerts: g.Alerts})
	}
	data["Groups"] = sums
	s.render(w, "overview", data)
}

// appResources is what the app page shows about the app's containers: graphs and a one-line summary.
// It is nil until the first check of the app has run.
type appResources struct {
	Summary string
	Charts  []chartView
}

func (s *Server) appResources(r *http.Request, app string) *appResources {
	ctx := r.Context()
	all, err := s.Store.ListServices(ctx)
	if err != nil {
		return nil
	}
	for _, v := range all {
		if v.Name != monitor.InternalName(app) {
			continue
		}
		st, _ := s.Store.ServiceState(ctx, v.ID)
		if st == nil || !st.Up {
			return nil
		}
		m := st.Latest
		res := &appResources{Charts: s.seriesCharts(ctx, v.ID, "container", []string{"cpu_pct", "mem_bytes"}, 24*time.Hour)}
		if int(m["containers_total"]) == 0 {
			res.Summary = "No containers yet: deploy the app to see its CPU and memory."
		} else {
			res.Summary = fmt.Sprintf("%d of %d containers running · CPU %s · memory %s · %.0f restarts. Graphs show the last 24 hours.",
				int(m["containers_running"]), int(m["containers_total"]), monitor.Format("pct_core", m["cpu_pct"]), monitor.Format("bytes", m["mem_bytes"]), m["restarts"])
		}
		return res
	}
	return nil
}
