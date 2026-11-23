// Package monitor checks supporting services (PostgreSQL, Redis, MongoDB, RabbitMQ, MySQL,
// HTTP and TCP endpoints), turns the results into samples, and raises alerts.
package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

// Spec is what a collector needs to reach one service (the password already decrypted).
type Spec struct {
	Name, Kind, Host string
	Port             int
	Username         string
	Password         string
	Params           map[string]string
}

// Result is one check.
type Result struct {
	Up      bool
	Err     string
	Info    map[string]string  // facts: version, uptime...
	Metrics map[string]float64 // gauges and values computed from counters
	Raw     map[string]float64 // raw counters, kept for the next check's rates
	Tables  []store.Table      // databases, queues, long queries...
}

// Prev is the previous check's raw counters, for rates.
type Prev struct {
	Raw map[string]float64
	At  time.Time
}

type CollectFunc func(ctx context.Context, sp Spec, prev Prev, now time.Time) Result

// Collector returns the check for a kind, or nil.
func Collector(kind string) CollectFunc {
	switch kind {
	case "postgres":
		return collectPostgres
	case "redis":
		return collectRedis
	case "mongo":
		return collectMongo
	case "rabbitmq":
		return collectRabbit
	case "mysql":
		return collectMySQL
	case "http":
		return collectHTTP
	case "tcp":
		return collectTCP
	}
	return nil
}

func newResult() Result {
	return Result{Info: map[string]string{}, Metrics: map[string]float64{}, Raw: map[string]float64{}}
}

func fail(err error) Result {
	r := newResult()
	r.Err = err.Error()
	return r
}

// rate turns a counter into a per-second rate using the previous check. ok is false on the first
// check, after a counter reset, or if no time has passed.
func rate(prev Prev, raw map[string]float64, key string, now time.Time) (float64, bool) {
	p, had := prev.Raw[key]
	secs := now.Sub(prev.At).Seconds()
	if !had || secs <= 0 {
		return 0, false
	}
	d := raw[key] - p
	if d < 0 {
		return 0, false
	}
	return d / secs, true
}

// delta is the change in a counter since the previous check (ok as for rate).
func delta(prev Prev, raw map[string]float64, key string) (float64, bool) {
	p, had := prev.Raw[key]
	if !had || raw[key] < p {
		return 0, false
	}
	return raw[key] - p, true
}

func humanBytes(b float64) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	div, exp := unit, 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", b/div, "KMGTP"[exp])
}

func humanDuration(secs float64) string {
	d := time.Duration(secs) * time.Second
	days := int(d.Hours()) / 24
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// Param returns a kind-specific setting or its default.
func Param(p map[string]string, key, def string) string {
	if v := strings.TrimSpace(p[key]); v != "" {
		return v
	}
	return def
}

// Threshold returns an alert threshold setting or its default.
func Threshold(p map[string]string, key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(p["t_"+key]), 64); err == nil {
		return v
	}
	return def
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
