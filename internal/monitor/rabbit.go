package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

var rabbitClient = &http.Client{Timeout: 8 * time.Second}

func rabbitGet(ctx context.Context, base *url.URL, sp Spec, path string, out any) error {
	u := *base
	u.Path, u.RawQuery, _ = strings.Cut(path, "?")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(sp.Username, sp.Password)
	resp, err := rabbitClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the management API refused the user (needs the 'monitoring' tag)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("management API returned %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// amqpProbe checks that something speaking AMQP 0-9-1 answers on the port.
func amqpProbe(ctx context.Context, sp Spec) (time.Duration, error) {
	c, lat, err := dial(ctx, sp.Host, sp.Port)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("AMQP\x00\x00\x09\x01")); err != nil {
		return 0, err
	}
	b := make([]byte, 7)
	if _, err := io.ReadFull(c, b); err != nil {
		return 0, fmt.Errorf("no AMQP answer: %w", err)
	}
	if b[0] != 1 && string(b[:4]) != "AMQP" { // a method frame (Connection.Start) or a protocol-version reply
		return 0, fmt.Errorf("that does not look like AMQP")
	}
	return lat, nil
}

func jnum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func jmap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func collectRabbit(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Is AMQP itself answering? (also the fallback when there is no management API)
	amqpLat, amqpErr := amqpProbe(ctx, sp)
	mgmtPort := Param(sp.Params, "mgmt_port", "15672")
	if mgmtPort == "0" || strings.EqualFold(mgmtPort, "off") {
		if amqpErr != nil {
			return fail(amqpErr)
		}
		r := newResult()
		r.Up = true
		r.Metrics["latency_ms"] = millis(amqpLat)
		r.Info["note"] = "management API off: only AMQP reachability is checked"
		return r
	}
	scheme := "http"
	if strings.EqualFold(Param(sp.Params, "mgmt_tls", "no"), "yes") {
		scheme = "https"
	}
	base := &url.URL{Scheme: scheme, Host: net.JoinHostPort(sp.Host, mgmtPort)}

	var overview map[string]any
	start := time.Now()
	if err := rabbitGet(ctx, base, sp, "/api/overview", &overview); err != nil {
		if amqpErr == nil && strings.Contains(err.Error(), "connection refused") { // plugin not enabled
			r := newResult()
			r.Up = true
			r.Metrics["latency_ms"] = millis(amqpLat)
			r.Info["note"] = "AMQP answers, but the management API is not reachable on port " + mgmtPort + ". Enable it (rabbitmq-plugins enable rabbitmq_management) for queue and memory metrics."
			return r
		}
		return fail(err)
	}
	r := newResult()
	r.Up = true // the management API answered, so the broker is running
	r.Metrics["latency_ms"] = millis(time.Since(start))
	if amqpErr != nil {
		r.Info["amqp"] = "management API answers, but AMQP on port " + strconv.Itoa(sp.Port) + " does not: " + amqpErr.Error()
	}
	r.Info["version"] = fmt.Sprint(overview["rabbitmq_version"])
	r.Info["erlang"] = fmt.Sprint(overview["erlang_version"])
	r.Info["node"] = fmt.Sprint(overview["node"])
	if v := overview["cluster_name"]; v != nil {
		r.Info["cluster"] = fmt.Sprint(v)
	}
	qt, ot, ms := jmap(overview["queue_totals"]), jmap(overview["object_totals"]), jmap(overview["message_stats"])
	r.Metrics["messages_ready"] = jnum(qt["messages_ready"])
	r.Metrics["messages_unacked"] = jnum(qt["messages_unacknowledged"])
	r.Metrics["connections"] = jnum(ot["connections"])
	r.Metrics["consumers"] = jnum(ot["consumers"])
	r.Metrics["publish_per_s"] = jnum(jmap(ms["publish_details"])["rate"])
	r.Metrics["deliver_per_s"] = jnum(jmap(ms["deliver_get_details"])["rate"])
	r.Info["queues"] = strconv.Itoa(int(jnum(ot["queues"])))

	var nodes []map[string]any
	if err := rabbitGet(ctx, base, sp, "/api/nodes", &nodes); err == nil && len(nodes) > 0 {
		n := nodes[0]
		memUsed, memLimit := jnum(n["mem_used"]), jnum(n["mem_limit"])
		r.Metrics["mem_used"] = memUsed
		if memLimit > 0 {
			r.Metrics["mem_pct"] = 100 * memUsed / memLimit
		}
		r.Metrics["disk_free"] = jnum(n["disk_free"])
		if fdTotal := jnum(n["fd_total"]); fdTotal > 0 {
			r.Metrics["fd_pct"] = 100 * jnum(n["fd_used"]) / fdTotal
		}
		alarm := 0.0
		if a, _ := n["mem_alarm"].(bool); a {
			alarm = 1
		}
		if a, _ := n["disk_free_alarm"].(bool); a {
			alarm = 1
		}
		r.Metrics["alarms"] = alarm
		if up := jnum(n["uptime"]); up > 0 {
			r.Info["uptime"] = humanDuration(up / 1000)
		}
	}

	// the queues with the most waiting messages
	var qs any
	if err := rabbitGet(ctx, base, sp, "/api/queues?page=1&page_size=15&sort=messages&sort_reverse=true&columns=name,vhost,messages_ready,messages_unacknowledged,consumers,state", &qs); err == nil {
		var items []any
		switch v := qs.(type) {
		case map[string]any:
			items, _ = v["items"].([]any)
		case []any:
			items = v
		}
		t := store.Table{Title: "Queues with the most messages", Columns: []string{"Queue", "Virtual host", "Ready", "Unacked", "Consumers", "State"}}
		for _, it := range items {
			q := jmap(it)
			t.Rows = append(t.Rows, []string{fmt.Sprint(q["name"]), fmt.Sprint(q["vhost"]), strconv.Itoa(int(jnum(q["messages_ready"]))),
				strconv.Itoa(int(jnum(q["messages_unacknowledged"]))), strconv.Itoa(int(jnum(q["consumers"]))), fmt.Sprint(q["state"])})
		}
		if len(t.Rows) > 0 {
			r.Tables = append(r.Tables, t)
		}
	}
	return r
}
