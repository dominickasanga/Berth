package monitor

import (
	"fmt"
	"math"
	"sort"
)

// MetricDef describes a metric for charts and cards.
type MetricDef struct {
	Name, Title, Unit string // Unit: count, pct, bytes, per_s, ms, bool
}

type ParamDef struct {
	Key, Label, Default, Help string
	Required                  bool
}

type ThresholdDef struct {
	Key, Label string
	Default    float64
	Unit       string
}

// KindDef is everything the UI needs to know about a kind of service.
type KindDef struct {
	Kind, Label     string
	DefaultPort     int
	HasCredentials  bool
	NeedsHost       bool
	Params          []ParamDef
	Thresholds      []ThresholdDef
	Metrics         []MetricDef
	Card            []string // the metrics shown on the status card
	CredentialsHelp string
	// Internal kinds are measured by Berth itself (the server, an app's containers). They are not
	// offered in the Add service form and are not listed among the services.
	Internal bool
}

var upMetric = MetricDef{"up", "Up", "bool"}

var kinds = map[string]KindDef{
	"postgres": {
		Kind: "postgres", Label: "PostgreSQL", DefaultPort: 5432, HasCredentials: true, NeedsHost: true,
		CredentialsHelp: "Use a read-only role: CREATE ROLE berth_monitor LOGIN PASSWORD '...'; GRANT pg_monitor TO berth_monitor;",
		Params: []ParamDef{
			{Key: "database", Label: "Database to connect to", Default: "postgres"},
			{Key: "sslmode", Label: "SSL mode", Default: "prefer", Help: "disable, prefer, require ..."},
			{Key: "long_query_secs", Label: "A query is 'long-running' after (seconds)", Default: "30"},
			{Key: "schema_sizes", Label: "List schema sizes (yes/no)", Default: "no", Help: "Lists schemas of the connected database with sizes (for example Kite360 tenants). Can be slow on large databases."},
		},
		Thresholds: []ThresholdDef{{"conn_pct", "Alert when connections used exceed", 85, "%"}, {"long_queries", "Alert at this many long-running queries", 3, ""}, {"waiting_locks", "Alert at this many waiting locks", 5, ""}},
		Metrics: []MetricDef{{"connections_pct", "Connections used", "pct"}, {"connections", "Connections", "count"}, {"connections_idle", "Idle connections", "count"}, {"idle_in_transaction", "Idle in transaction", "count"}, {"tps", "Transactions per second", "per_s"},
			{"cache_hit_pct", "Cache hit ratio", "pct"}, {"db_size_bytes", "Total database size", "bytes"}, {"long_queries", "Long-running queries", "count"},
			{"waiting_locks", "Waiting locks", "count"}, {"deadlocks_per_min", "Deadlocks per minute", "count"}},
		Card: []string{"connections_pct", "cache_hit_pct", "tps", "db_size_bytes"},
	},
	"redis": {
		Kind: "redis", Label: "Redis", DefaultPort: 6379, HasCredentials: true, NeedsHost: true,
		CredentialsHelp: "Optional. Leave empty if Redis has no password. A user with only +info +ping is enough.",
		Thresholds:      []ThresholdDef{{"mem_pct", "Alert when memory used exceeds (needs maxmemory)", 90, "%"}},
		Metrics: []MetricDef{{"used_memory", "Memory used", "bytes"}, {"memory_pct", "Memory of maxmemory", "pct"}, {"connected_clients", "Connected clients", "count"},
			{"ops_per_s", "Operations per second", "per_s"}, {"hit_pct", "Keyspace hit ratio", "pct"}, {"evicted_per_s", "Evictions per second", "per_s"},
			{"keys_total", "Keys", "count"}, {"latency_ms", "PING latency", "ms"}},
		Card: []string{"used_memory", "connected_clients", "ops_per_s", "hit_pct"},
	},
	"mongo": {
		Kind: "mongo", Label: "MongoDB", DefaultPort: 27017, HasCredentials: true, NeedsHost: true,
		CredentialsHelp: "Optional. A user with the clusterMonitor role is enough.",
		Params: []ParamDef{
			{Key: "auth_source", Label: "Authentication database", Default: "admin"},
			{Key: "tls", Label: "Use TLS (yes/no)", Default: "no"},
		},
		Thresholds: []ThresholdDef{{"conn_pct", "Alert when connections used exceed", 80, "%"}},
		Metrics: []MetricDef{{"connections_pct", "Connections used", "pct"}, {"connections_current", "Connections", "count"}, {"ops_per_s", "Operations per second", "per_s"},
			{"mem_resident_bytes", "Resident memory", "bytes"}, {"db_size_bytes", "Total database size", "bytes"}, {"latency_ms", "Ping latency", "ms"}},
		Card: []string{"connections_current", "ops_per_s", "mem_resident_bytes", "db_size_bytes"},
	},
	"rabbitmq": {
		Kind: "rabbitmq", Label: "RabbitMQ", DefaultPort: 5672, HasCredentials: true, NeedsHost: true,
		CredentialsHelp: "Needs a user tagged 'monitoring' for the management API, plus an empty permission on each virtual host to list queues: rabbitmqctl set_permissions -p / <user> '^$' '^$' '^$' (matches nothing, so it cannot read or publish). Without the management plugin Berth can only check that AMQP answers.",
		Params: []ParamDef{
			{Key: "mgmt_port", Label: "Management API port", Default: "15672", Help: "Enable it with: rabbitmq-plugins enable rabbitmq_management"},
			{Key: "mgmt_tls", Label: "Management API over HTTPS (yes/no)", Default: "no"},
		},
		Thresholds: []ThresholdDef{{"queue_ready", "Alert when messages waiting exceed", 10000, ""}},
		Metrics: []MetricDef{{"messages_ready", "Messages ready", "count"}, {"messages_unacked", "Messages unacknowledged", "count"}, {"publish_per_s", "Published per second", "per_s"},
			{"deliver_per_s", "Delivered per second", "per_s"}, {"connections", "Connections", "count"}, {"consumers", "Consumers", "count"},
			{"mem_used", "Memory used", "bytes"}, {"mem_pct", "Memory of its limit", "pct"}, {"disk_free", "Disk free", "bytes"}, {"fd_pct", "File descriptors used", "pct"},
			{"alarms", "Resource alarms (1 = active)", "bool"}, {"latency_ms", "Response time", "ms"}},
		Card: []string{"messages_ready", "connections", "publish_per_s", "mem_used"},
	},
	"mysql": {
		Kind: "mysql", Label: "MySQL (reachability)", DefaultPort: 3306, NeedsHost: true,
		CredentialsHelp: "No credentials needed: Berth reads the server's greeting to confirm MySQL answers and report its version.",
		Metrics:         []MetricDef{{"latency_ms", "Connect time", "ms"}},
		Card:            []string{"latency_ms"},
	},
	"http": {
		Kind: "http", Label: "HTTP endpoint", HasCredentials: true,
		CredentialsHelp: "Optional basic-auth user and password.",
		Params: []ParamDef{
			{Key: "url", Label: "URL", Required: true, Help: "For example http://127.0.0.1:8787/health"},
			{Key: "expect", Label: "Expected status code", Default: "", Help: "Empty means any status below 400"},
		},
		Thresholds: []ThresholdDef{{"latency_ms", "Alert when the response takes longer than", 2000, "ms"}},
		Metrics:    []MetricDef{{"latency_ms", "Response time", "ms"}, {"status_code", "Status code", "count"}},
		Card:       []string{"latency_ms", "status_code"},
	},
	"system": {
		Kind: "system", Label: "Server", Internal: true,
		Thresholds: []ThresholdDef{{"disk_pct", "Alert when a disk is fuller than", 85, "%"}, {"mem_pct", "Alert when memory in use exceeds", 90, "%"}},
		Metrics: []MetricDef{{"cpu_pct", "CPU", "pct"}, {"mem_pct", "Memory", "pct"}, {"mem_used_bytes", "Memory used", "bytes"}, {"disk_pct", "Fullest disk", "pct"},
			{"net_rx_bytes_s", "Network in", "bytes_s"}, {"net_tx_bytes_s", "Network out", "bytes_s"}, {"load1", "Load (1 min)", "count"}},
		Card: []string{"cpu_pct", "mem_pct", "disk_pct", "load1"},
	},
	"container": {
		Kind: "container", Label: "App containers", Internal: true,
		Metrics: []MetricDef{{"cpu_pct", "CPU", "pct_core"}, {"mem_bytes", "Memory", "bytes"}, {"restarts", "Restarts", "count"}, {"containers_running", "Containers running", "count"}},
		Card:    []string{"containers_running", "cpu_pct", "mem_bytes", "restarts"},
	},
	"tcp": {
		Kind: "tcp", Label: "TCP port", NeedsHost: true,
		CredentialsHelp: "Only checks that the port accepts a connection.",
		Metrics:         []MetricDef{{"latency_ms", "Connect time", "ms"}},
		Card:            []string{"latency_ms"},
	},
}

// Kind returns the definition of a kind.
func Kind(k string) (KindDef, bool) { d, ok := kinds[k]; return d, ok }

// KindNames lists the kinds in a stable order.
func KindNames() []string {
	out := make([]string, 0, len(kinds))
	for k, d := range kinds {
		if !d.Internal { // Berth's own server and container checks are not something to add by hand
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// MetricsFor lists a kind's metrics, with "up" first.
func MetricsFor(kind string) []MetricDef {
	return append([]MetricDef{upMetric}, kinds[kind].Metrics...)
}

// MetricUnit returns a metric's unit.
func MetricUnit(kind, name string) string {
	for _, m := range MetricsFor(kind) {
		if m.Name == name {
			return m.Unit
		}
	}
	return "count"
}

// Format renders a metric value for display.
func Format(unit string, v float64) string {
	switch unit {
	case "pct":
		return fmt.Sprintf("%.1f%%", v)
	case "bytes":
		return humanBytes(v)
	case "per_s":
		return fmt.Sprintf("%.1f/s", v)
	case "bytes_s":
		return humanBytes(v) + "/s"
	case "pct_core": // docker's CPU figure: 100% is one full core, so it can exceed 100
		return fmt.Sprintf("%.1f%%", v)
	case "ms":
		if v < 10 {
			return fmt.Sprintf("%.1f ms", v)
		}
		return fmt.Sprintf("%.0f ms", v)
	case "bool":
		if v >= 0.5 {
			return "yes"
		}
		return "no"
	}
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}
