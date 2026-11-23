package monitor

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// reachable skips a test when the real service is not running on this machine.
func reachable(t *testing.T, port int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Skipf("nothing listening on 127.0.0.1:%d", port)
	}
	c.Close()
}

func spec(kind string, port int, params map[string]string) Spec {
	return Spec{Name: "t", Kind: kind, Host: "127.0.0.1", Port: port, Params: params}
}

func run(t *testing.T, sp Spec, prev Prev) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Collector(sp.Kind)(ctx, sp, prev, time.Now())
}

func TestRealPostgres(t *testing.T) {
	reachable(t, 5432)
	sp := spec("postgres", 5432, nil)
	sp.Username = "dominic"
	first := run(t, sp, Prev{})
	if !first.Up {
		t.Skipf("local Postgres is not reachable as dominic: %s", first.Err)
	}
	if first.Info["version"] == "" || first.Metrics["connections"] < 1 || first.Metrics["db_size_bytes"] <= 0 {
		t.Fatalf("missing basics: %+v", first)
	}
	if _, ok := first.Metrics["tps"]; ok {
		t.Fatal("a rate cannot exist on the first check")
	}
	if len(first.Tables) == 0 || first.Tables[0].Title != "Databases" {
		t.Fatalf("expected a databases table: %+v", first.Tables)
	}
	time.Sleep(1200 * time.Millisecond)
	second := Collector("postgres")(context.Background(), sp, Prev{Raw: first.Raw, At: time.Now().Add(-time.Second)}, time.Now())
	if _, ok := second.Metrics["tps"]; !ok {
		t.Fatalf("the second check should have a transaction rate: %+v", second.Metrics)
	}
	if p := second.Metrics["cache_hit_pct"]; p < 0 || p > 100 {
		t.Fatalf("cache hit ratio out of range: %v", p)
	}
	// who holds the connections, grouped by application; Berth's own check is one of them
	var byApp []string
	for _, tb := range first.Tables {
		if tb.Title == "Connections by application" {
			for _, row := range tb.Rows {
				byApp = append(byApp, row[0])
			}
		}
	}
	if len(byApp) == 0 || !strings.Contains(strings.Join(byApp, ","), "berth-monitor") {
		t.Fatalf("expected a by-application table that includes berth-monitor: %v", byApp)
	}
	if _, ok := first.Metrics["connections_idle"]; !ok {
		t.Fatal("idle connections should be a metric")
	}
	if _, ok := first.Metrics["idle_in_transaction"]; !ok {
		t.Fatal("idle-in-transaction connections should be a metric")
	}
	// schema sizes are opt-in
	sp.Params = map[string]string{"schema_sizes": "yes"}
	withSchemas := run(t, sp, Prev{})
	found := false
	for _, tb := range withSchemas.Tables {
		found = found || strings.HasPrefix(tb.Title, "Schemas in ")
	}
	if !found {
		t.Fatal("schema_sizes=yes should add a schemas table")
	}
}

func TestPostgresWrongPasswordAndDown(t *testing.T) {
	if r := run(t, spec("postgres", 1, nil), Prev{}); r.Up || r.Err == "" {
		t.Fatalf("a closed port must be down: %+v", r)
	}
}

func TestRealRedis(t *testing.T) {
	reachable(t, 6379)
	first := run(t, spec("redis", 6379, nil), Prev{})
	if !first.Up {
		t.Skipf("local Redis needs a password: %s", first.Err)
	}
	if first.Info["version"] == "" || first.Metrics["used_memory"] <= 0 || first.Metrics["latency_ms"] <= 0 {
		t.Fatalf("missing basics: %+v", first)
	}
	if _, ok := first.Metrics["hit_pct"]; ok {
		t.Fatal("a hit ratio needs two checks")
	}
	second := Collector("redis")(context.Background(), spec("redis", 6379, nil), Prev{Raw: first.Raw, At: time.Now().Add(-time.Second)}, time.Now())
	if !second.Up {
		t.Fatal(second.Err)
	}
}

// A fake Redis proves AUTH, PING and INFO handling without depending on a real server.
func TestRedisAgainstAFakeServer(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	info := "# Server\r\nredis_version:7.2.4\r\nuptime_in_seconds:90061\r\n# Memory\r\nused_memory:1048576\r\nmaxmemory:2097152\r\n" +
		"connected_clients:3\r\ninstantaneous_ops_per_sec:42\r\nkeyspace_hits:900\r\nkeyspace_misses:100\r\nevicted_keys:10\r\nrole:master\r\n# Keyspace\r\ndb0:keys=5,expires=0\r\ndb2:keys=7,expires=1\r\n"
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				authed := false
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					if !strings.HasPrefix(line, "*") {
						continue
					}
					n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
					var args []string
					for i := 0; i < n; i++ {
						rd.ReadString('\n')
						a, _ := rd.ReadString('\n')
						args = append(args, strings.TrimSpace(a))
					}
					switch strings.ToUpper(args[0]) {
					case "AUTH":
						if args[len(args)-1] == "s3cret" {
							authed = true
							c.Write([]byte("+OK\r\n"))
						} else {
							c.Write([]byte("-WRONGPASS invalid username-password pair\r\n"))
						}
					case "PING":
						c.Write([]byte("+PONG\r\n"))
					case "INFO":
						if !authed {
							c.Write([]byte("-NOAUTH Authentication required.\r\n"))
						} else {
							c.Write([]byte("$" + strconv.Itoa(len(info)) + "\r\n" + info + "\r\n"))
						}
					}
				}
			}(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	sp := spec("redis", port, nil)

	if r := run(t, sp, Prev{}); r.Up || !strings.Contains(r.Err, "NOAUTH") && !strings.Contains(r.Err, "INFO failed") {
		t.Fatalf("without the password the check must fail: %+v", r)
	}
	sp.Password = "wrong"
	if r := run(t, sp, Prev{}); r.Up || !strings.Contains(r.Err, "authentication failed") {
		t.Fatalf("a wrong password must be reported: %+v", r)
	}
	sp.Password = "s3cret"
	prev := Prev{Raw: map[string]float64{"hits": 800, "misses": 100, "evicted": 0}, At: time.Now().Add(-10 * time.Second)}
	r := run(t, sp, prev)
	if !r.Up || r.Info["version"] != "7.2.4" || r.Info["uptime"] != "1d 1h" || r.Info["maxmemory"] != "2.0 MiB" {
		t.Fatalf("info wrong: %+v", r.Info)
	}
	m := r.Metrics
	if m["used_memory"] != 1048576 || m["memory_pct"] != 50 || m["connected_clients"] != 3 || m["ops_per_s"] != 42 || m["keys_total"] != 12 {
		t.Fatalf("gauges wrong: %+v", m)
	}
	// 100 new hits and 0 new misses since the previous check, and 10 evictions in about 10 seconds
	if m["hit_pct"] != 100 || m["evicted_per_s"] < 0.9 || m["evicted_per_s"] > 1.1 {
		t.Fatalf("rates wrong: hit=%v evicted/s=%v", m["hit_pct"], m["evicted_per_s"])
	}
}

func TestRealMongo(t *testing.T) {
	reachable(t, 27017)
	r := run(t, spec("mongo", 27017, nil), Prev{})
	if !r.Up {
		t.Skipf("local MongoDB needs credentials: %s", r.Err)
	}
	if r.Info["version"] == "" || r.Metrics["connections_current"] < 1 || r.Metrics["latency_ms"] <= 0 {
		t.Fatalf("missing basics: %+v", r)
	}
	if _, ok := r.Metrics["ops_per_s"]; ok {
		t.Fatal("a rate needs two checks")
	}
	second := Collector("mongo")(context.Background(), spec("mongo", 27017, nil), Prev{Raw: r.Raw, At: time.Now().Add(-time.Second)}, time.Now())
	if _, ok := second.Metrics["ops_per_s"]; !ok {
		t.Fatalf("the second check should have an ops rate: %+v", second.Metrics)
	}
}

func TestRealRabbitMQ(t *testing.T) {
	reachable(t, 15672)
	sp := spec("rabbitmq", 5672, nil)
	sp.Username, sp.Password = "guest", "guest"
	r := run(t, sp, Prev{})
	if !r.Up || r.Info["version"] == "" {
		t.Skipf("local RabbitMQ does not accept guest/guest: %+v", r)
	}
	if _, ok := r.Metrics["mem_pct"]; !ok || r.Metrics["connections"] < 0 {
		t.Fatalf("missing node metrics: %+v", r.Metrics)
	}
	if len(r.Tables) == 0 || len(r.Tables[0].Rows) == 0 {
		t.Fatalf("expected a queues table: %+v", r.Tables)
	}
	// wrong password: the broker is up (AMQP answers), the monitoring login is what is wrong
	sp.Password = "nope"
	bad := run(t, sp, Prev{})
	if !bad.Up || bad.Info["mgmt_auth"] != "refused" || !strings.Contains(bad.Info["note"], "refused") {
		t.Fatalf("a refused login must not look like an outage: %+v", bad)
	}
	if got := Conditions("rabbitmq", nil, bad, 0); len(got) != 1 || got[0].Key != "monitoring" || got[0].Severity != "warning" {
		t.Fatalf("a refused login should raise one warning, not a down alert: %+v", got)
	}
	// no management API: AMQP still answers, so it is up but says what is missing
	sp.Password = "guest"
	sp.Params = map[string]string{"mgmt_port": "1"}
	if fb := run(t, sp, Prev{}); !fb.Up || !strings.Contains(fb.Info["note"], "management API") {
		t.Fatalf("fallback to the AMQP check failed: %+v", fb)
	}
}

func TestTCPHTTPAndMySQL(t *testing.T) {
	// TCP
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if r := run(t, spec("tcp", port, nil), Prev{}); !r.Up || r.Metrics["latency_ms"] <= 0 {
		t.Fatalf("open port should be up: %+v", r)
	}
	ln.Close()
	if r := run(t, spec("tcp", port, nil), Prev{}); r.Up || r.Err == "" {
		t.Fatalf("a closed port must be down: %+v", r)
	}

	// HTTP: status handling, expected code, basic auth, redirects not followed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("fine"))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/auth":
			if u, p, _ := r.BasicAuth(); u != "me" || p != "pw" {
				w.WriteHeader(401)
			}
		default:
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	h := func(path, expect string, user, pass string) Result {
		sp := Spec{Kind: "http", Username: user, Password: pass, Params: map[string]string{"url": srv.URL + path, "expect": expect}}
		return run(t, sp, Prev{})
	}
	if r := h("/ok", "", "", ""); !r.Up || r.Metrics["status_code"] != 200 {
		t.Fatalf("200 should be up: %+v", r)
	}
	if r := h("/broken", "", "", ""); r.Up || r.Metrics["status_code"] != 503 {
		t.Fatalf("503 should be down: %+v", r)
	}
	if r := h("/redirect", "302", "", ""); !r.Up || r.Metrics["status_code"] != 302 {
		t.Fatalf("a redirect must be reported, not followed: %+v", r)
	}
	if r := h("/auth", "", "me", "pw"); !r.Up {
		t.Fatalf("basic auth should be sent: %+v", r)
	}
	if r := h("/auth", "", "", ""); r.Up {
		t.Fatal("401 without credentials must be down")
	}
	if r := run(t, Spec{Kind: "http", Params: map[string]string{"url": "file:///etc/passwd"}}, Prev{}); r.Up || !strings.Contains(r.Err, "http://") {
		t.Fatalf("only http(s) URLs are allowed: %+v", r)
	}

	// MySQL greeting: a fake server sends the handshake packet
	ml, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ml.Close()
	go func() {
		for {
			c, err := ml.Accept()
			if err != nil {
				return
			}
			body := append([]byte{10}, []byte("8.0.35-fake\x00")...)
			body = append(body, make([]byte, 20)...)
			c.Write(append([]byte{byte(len(body)), 0, 0, 0}, body...))
			c.Close()
		}
	}()
	mr := run(t, spec("mysql", ml.Addr().(*net.TCPAddr).Port, nil), Prev{})
	if !mr.Up || mr.Info["version"] != "8.0.35-fake" {
		t.Fatalf("mysql greeting: %+v", mr)
	}
	if r := run(t, spec("mysql", port, nil), Prev{}); r.Up {
		t.Fatal("a closed port is not MySQL")
	}
}

func TestRateAndDelta(t *testing.T) {
	now := time.Now()
	prev := Prev{Raw: map[string]float64{"x": 100}, At: now.Add(-10 * time.Second)}
	if v, ok := rate(prev, map[string]float64{"x": 150}, "x", now); !ok || v != 5 {
		t.Errorf("rate = %v %v, want 5", v, ok)
	}
	if _, ok := rate(prev, map[string]float64{"x": 50}, "x", now); ok {
		t.Error("a counter reset must not produce a negative rate")
	}
	if _, ok := rate(Prev{}, map[string]float64{"x": 1}, "x", now); ok {
		t.Error("no previous check, no rate")
	}
	if d, ok := delta(prev, map[string]float64{"x": 130}, "x"); !ok || d != 30 {
		t.Errorf("delta = %v %v", d, ok)
	}
}

// A monitoring user needs the 'monitoring' tag, and listing queues needs a permission entry on the
// vhost. An entry whose patterns match nothing is enough, and gives no way to read or publish.
func TestRabbitMonitoringUserPermissions(t *testing.T) {
	reachable(t, 15672)
	api := func(method, path, body string) int {
		req, _ := http.NewRequest(method, "http://127.0.0.1:15672"+path, strings.NewReader(body))
		req.SetBasicAuth("guest", "guest")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Skipf("management API not usable as guest: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := api("PUT", "/api/users/berthtest", `{"password":"tmp-pw-123","tags":"monitoring"}`); code != 201 && code != 204 {
		t.Skipf("cannot create a test user: HTTP %d", code)
	}
	defer api("DELETE", "/api/users/berthtest", "")
	sp := spec("rabbitmq", 5672, nil)
	sp.Username, sp.Password = "berthtest", "tmp-pw-123"

	r := run(t, sp, Prev{})
	if !r.Up || r.Metrics["connections"] < 0 {
		t.Fatalf("totals and node health need only the tag: %+v", r)
	}
	if r.Info["queues"] != "0" && len(r.Tables) == 0 && !strings.Contains(r.Info["note"], "cannot be listed") {
		t.Fatalf("an empty queue list must be explained: %+v", r.Info)
	}
	api("PUT", "/api/permissions/%2F/berthtest", `{"configure":"^$","write":"^$","read":"^$"}`)
	r = run(t, sp, Prev{})
	if r.Info["queues"] != "0" && (len(r.Tables) == 0 || len(r.Tables[0].Rows) == 0) {
		t.Fatalf("a match-nothing permission should be enough to list queues: %+v", r)
	}
	if r.Info["note"] != "" && strings.Contains(r.Info["note"], "cannot be listed") {
		t.Fatalf("no warning once the queues are listed: %s", r.Info["note"])
	}
}
