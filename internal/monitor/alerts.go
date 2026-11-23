package monitor

import (
	"fmt"
	"strconv"
	"strings"
)

// Condition is something wrong right now. An alert stays open while its condition holds.
type Condition struct {
	Key, Severity, Message string
}

// FailChecks is how many failed checks in a row make a service "down" (default 2).
func failChecks(p map[string]string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(p["fail_checks"])); err == nil && n > 0 {
		return n
	}
	return 2
}

// Conditions lists what is wrong for one check. failures counts consecutive failed checks.
func Conditions(kind string, params map[string]string, r Result, failures int) []Condition {
	if !r.Up {
		if failures >= failChecks(params) {
			return []Condition{{"down", "critical", "is down: " + r.Err}}
		}
		return nil // one failed check can be a blip; wait for the next
	}
	m := r.Metrics
	var out []Condition
	add := func(key, sev, format string, a ...any) {
		out = append(out, Condition{key, sev, fmt.Sprintf(format, a...)})
	}
	switch kind {
	case "postgres":
		if t := Threshold(params, "conn_pct", 85); m["connections_pct"] > t {
			add("connections", "warning", "%.0f%% of max_connections in use (%.0f of %s), threshold %.0f%%", m["connections_pct"], m["connections"], r.Info["max_connections"], t)
		}
		if t := Threshold(params, "long_queries", 3); t > 0 && m["long_queries"] >= t {
			add("long_queries", "warning", "%.0f queries have been running longer than %ss", m["long_queries"], Param(params, "long_query_secs", "30"))
		}
		if t := Threshold(params, "waiting_locks", 5); t > 0 && m["waiting_locks"] >= t {
			add("locks", "warning", "%.0f sessions are waiting for locks", m["waiting_locks"])
		}
	case "redis":
		if t := Threshold(params, "mem_pct", 90); m["memory_pct"] > t {
			add("memory", "warning", "memory is at %.0f%% of maxmemory (threshold %.0f%%)", m["memory_pct"], t)
		}
	case "mongo":
		if t := Threshold(params, "conn_pct", 80); m["connections_pct"] > t {
			add("connections", "warning", "%.0f%% of the available connections are in use (threshold %.0f%%)", m["connections_pct"], t)
		}
	case "rabbitmq":
		if r.Info["mgmt_auth"] == "refused" {
			add("monitoring", "warning", "the management API refused the login, so queue and memory metrics are missing (RabbitMQ itself answers)")
		}
		if m["alarms"] >= 1 {
			add("alarm", "critical", "a memory or disk alarm is active: RabbitMQ is blocking publishers")
		}
		if t := Threshold(params, "queue_ready", 10000); t > 0 && m["messages_ready"] > t {
			add("backlog", "warning", "%.0f messages are waiting in queues (threshold %.0f)", m["messages_ready"], t)
		}
	case "http":
		if t := Threshold(params, "latency_ms", 2000); t > 0 && m["latency_ms"] > t {
			add("slow", "warning", "responded in %.0f ms (threshold %.0f ms)", m["latency_ms"], t)
		}
	}
	return out
}
