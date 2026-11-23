package monitor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func dial(ctx context.Context, host string, port int) (net.Conn, time.Duration, error) {
	start := time.Now()
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	return c, time.Since(start), err
}

func collectTCP(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	c, lat, err := dial(ctx, sp.Host, sp.Port)
	if err != nil {
		return fail(err)
	}
	c.Close()
	r := newResult()
	r.Up = true
	r.Metrics["latency_ms"] = millis(lat)
	return r
}

// collectMySQL reads the greeting MySQL sends before any login, so no credentials are needed.
func collectMySQL(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	c, lat, err := dial(ctx, sp.Host, sp.Port)
	if err != nil {
		return fail(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	head := make([]byte, 5) // 3 bytes length, 1 sequence, 1 protocol version (or 0xff for an error)
	if _, err := io.ReadFull(c, head); err != nil {
		return fail(fmt.Errorf("no MySQL greeting: %w", err))
	}
	n := int(head[0]) | int(head[1])<<8 | int(head[2])<<16
	if n < 2 || n > 1<<16 {
		return fail(fmt.Errorf("that does not look like MySQL"))
	}
	body := make([]byte, n-1)
	if _, err := io.ReadFull(c, body); err != nil {
		return fail(err)
	}
	if head[4] == 0xff { // an error packet: the server answered but refuses us
		msg := ""
		if len(body) > 2 {
			msg = strings.TrimSpace(string(body[2:]))
		}
		return fail(fmt.Errorf("MySQL refused the connection: %s", msg))
	}
	if head[4] != 10 {
		return fail(fmt.Errorf("unexpected MySQL protocol version %d", head[4]))
	}
	r := newResult()
	r.Up = true
	r.Metrics["latency_ms"] = millis(lat)
	if i := strings.IndexByte(string(body), 0); i > 0 {
		r.Info["version"] = string(body[:i])
	}
	return r
}

var noRedirect = &http.Client{
	Timeout:       8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func collectHTTP(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	u, err := url.Parse(Param(sp.Params, "url", ""))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fail(fmt.Errorf("the URL must start with http:// or https://"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fail(err)
	}
	if sp.Username != "" {
		req.SetBasicAuth(sp.Username, sp.Password)
	}
	req.Header.Set("User-Agent", "berth-monitor")
	start := time.Now()
	resp, err := noRedirect.Do(req)
	lat := time.Since(start)
	if err != nil {
		return fail(err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	r := newResult()
	r.Metrics["latency_ms"] = millis(lat)
	r.Metrics["status_code"] = float64(resp.StatusCode)
	r.Info["status"] = resp.Status
	r.Up = resp.StatusCode < 400
	if want := Param(sp.Params, "expect", ""); want != "" {
		r.Up = strconv.Itoa(resp.StatusCode) == want
	}
	if !r.Up {
		r.Err = "unexpected status " + resp.Status
	}
	return r
}

// ---- Redis (a small RESP client, so Berth needs no Redis library) ----

func respCommand(w io.Writer, args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// respReply reads one simple-string, integer or bulk-string reply; an error reply becomes an error.
func respReply(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", fmt.Errorf("empty reply")
	}
	switch line[0] {
	case '+', ':':
		return line[1:], nil
	case '-':
		return "", fmt.Errorf("%s", line[1:])
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 || n > 8<<20 {
			return "", fmt.Errorf("bad bulk length %q", line)
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	}
	return "", fmt.Errorf("unexpected reply %q", line)
}

func collectRedis(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	c, _, err := dial(ctx, sp.Host, sp.Port)
	if err != nil {
		return fail(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	rd := bufio.NewReader(c)
	if sp.Password != "" {
		args := []string{"AUTH", sp.Password}
		if sp.Username != "" {
			args = []string{"AUTH", sp.Username, sp.Password}
		}
		if respCommand(c, args...) != nil {
			return fail(fmt.Errorf("write failed"))
		}
		if _, err := respReply(rd); err != nil {
			return fail(fmt.Errorf("authentication failed: %w", err))
		}
	}
	start := time.Now()
	respCommand(c, "PING")
	if pong, err := respReply(rd); err != nil || pong != "PONG" {
		return fail(fmt.Errorf("PING failed: %v %s", err, pong))
	}
	lat := time.Since(start)
	respCommand(c, "INFO")
	text, err := respReply(rd)
	if err != nil {
		return fail(fmt.Errorf("INFO failed: %w", err))
	}
	return redisResult(text, lat, prev, now)
}

func redisResult(info string, lat time.Duration, prev Prev, now time.Time) Result {
	kv := map[string]string{}
	keys := 0.0
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kv[k] = v
		if len(k) > 2 && k[:2] == "db" && strings.Contains(v, "keys=") { // db0:keys=12,expires=0,...
			for _, part := range strings.Split(v, ",") {
				if n, ok := strings.CutPrefix(part, "keys="); ok {
					f, _ := strconv.ParseFloat(n, 64)
					keys += f
				}
			}
		}
	}
	num := func(k string) float64 { f, _ := strconv.ParseFloat(kv[k], 64); return f }
	r := newResult()
	r.Up = true
	r.Metrics["latency_ms"] = millis(lat)
	r.Metrics["used_memory"] = num("used_memory")
	r.Metrics["connected_clients"] = num("connected_clients")
	r.Metrics["ops_per_s"] = num("instantaneous_ops_per_sec")
	r.Metrics["keys_total"] = keys
	if max := num("maxmemory"); max > 0 {
		r.Metrics["memory_pct"] = 100 * num("used_memory") / max
	}
	r.Raw["hits"], r.Raw["misses"], r.Raw["evicted"] = num("keyspace_hits"), num("keyspace_misses"), num("evicted_keys")
	if dh, ok := delta(prev, r.Raw, "hits"); ok {
		dm, _ := delta(prev, r.Raw, "misses")
		if dh+dm > 0 {
			r.Metrics["hit_pct"] = 100 * dh / (dh + dm)
		}
	}
	if v, ok := rate(prev, r.Raw, "evicted", now); ok {
		r.Metrics["evicted_per_s"] = v
	}
	r.Info["version"] = kv["redis_version"]
	r.Info["role"] = kv["role"]
	if m := kv["redis_mode"]; m != "" {
		r.Info["mode"] = m
	}
	r.Info["uptime"] = humanDuration(num("uptime_in_seconds"))
	if max := num("maxmemory"); max > 0 {
		r.Info["maxmemory"] = humanBytes(max)
	} else {
		r.Info["maxmemory"] = "no limit"
	}
	return r
}
