package monitor

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeMail struct {
	mu     sync.Mutex
	sent   []string
	bodies []string
}

func (f *fakeMail) Send(subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, subject)
	f.bodies = append(f.bodies, body)
	return nil
}
func (f *fakeMail) subjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func testSampler(t *testing.T) (*Sampler, *fakeMail, context.Context) {
	dsn := os.Getenv("BERTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BERTH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Own schema: the web tests reset "public" in the same database, in parallel with these.
	if _, err := raw.Exec(ctx, `DROP SCHEMA IF EXISTS monitor_test CASCADE; CREATE SCHEMA monitor_test`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	st, err := store.Open(ctx, dsn+sep+"search_path=monitor_test")
	if err != nil {
		t.Fatal(err)
	}
	box, _ := crypt.New(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	m := &fakeMail{}
	return &Sampler{Store: st, Box: box, Mail: m}, m, ctx
}

func TestDownAlertFiresOnceAndResolves(t *testing.T) {
	s, mail, ctx := testSampler(t)
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
	id, err := s.Store.CreateService(ctx, store.Service{Name: "queue", Kind: "tcp", Host: "127.0.0.1", Port: port, Enabled: true, Params: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := s.Store.ServiceByID(ctx, id)

	check := func() *store.ServiceState { return s.CheckService(ctx, *svc) }
	if st := check(); !st.Up || st.Failures != 0 {
		t.Fatalf("a listening port is up: %+v", st)
	}
	ln.Close() // the service goes away

	// one failed check is not enough (default: 2 in a row)
	if st := check(); st.Up || st.Failures != 1 {
		t.Fatalf("first failure: %+v", st)
	}
	if open, _ := s.Store.OpenAlerts(ctx); len(open) != 0 || len(mail.subjects()) != 0 {
		t.Fatalf("one blip must not alert: alerts=%d mails=%v", len(open), mail.subjects())
	}
	// the second failure opens exactly one alert and sends exactly one mail
	check()
	open, _ := s.Store.OpenAlerts(ctx)
	if len(open) != 1 || open[0].Key != "down" || open[0].Severity != "critical" {
		t.Fatalf("expected one down alert: %+v", open)
	}
	if got := mail.subjects(); len(got) != 1 || !strings.HasPrefix(got[0], "[Berth] FIRING: queue (down)") {
		t.Fatalf("firing mail: %v", got)
	}
	if body := mail.bodies[0]; !strings.HasPrefix(body, "queue is down: ") || strings.Contains(body, "down is down") {
		t.Fatalf("the mail should open with a readable sentence: %q", body)
	}
	// still down: no new alert, no new mail
	check()
	check()
	if open, _ := s.Store.OpenAlerts(ctx); len(open) != 1 || len(mail.subjects()) != 1 {
		t.Fatalf("a standing problem must not repeat: alerts=%d mails=%v", len(open), mail.subjects())
	}
	// the state keeps "since" from the moment it went down
	stDown, _ := s.Store.ServiceState(ctx, id)
	if stDown.Up || stDown.Failures < 4 || stDown.Error == "" {
		t.Fatalf("down state: %+v", stDown)
	}

	// it comes back: the alert resolves and one recovery mail goes out
	ln2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Skip("could not reuse the port")
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if st := check(); !st.Up || st.Failures != 0 {
		t.Fatalf("recovered: %+v", st)
	}
	if open, _ := s.Store.OpenAlerts(ctx); len(open) != 0 {
		t.Fatalf("the alert should have resolved: %+v", open)
	}
	if got := mail.subjects(); len(got) != 2 || !strings.HasPrefix(got[1], "[Berth] RESOLVED: queue (down)") {
		t.Fatalf("recovery mail: %v", got)
	}
	if !stDown.Since.Before(time.Now()) {
		t.Fatal("since should be in the past")
	}
	// samples: the raw "up" series recorded both states (a chart averages these into buckets)
	var ups, downs int
	s.Store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE value = 1), count(*) FILTER (WHERE value = 0)
		FROM metric_samples WHERE service_id = $1 AND metric = 'up'`, id).Scan(&ups, &downs)
	if ups < 2 || downs < 4 {
		t.Fatalf("expected up and down samples to be recorded: up=%d down=%d", ups, downs)
	}
	if pts, _ := s.Store.Series(ctx, id, "up", time.Now().Add(-time.Hour), 3600); len(pts) != 1 || pts[0].V <= 0 || pts[0].V >= 1 {
		t.Fatalf("an hour bucket should average the series between 0 and 1: %+v", pts)
	}
}

func TestThresholdAlertAndMailRetry(t *testing.T) {
	s, mail, ctx := testSampler(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
	}))
	defer slow.Close()
	id, _ := s.Store.CreateService(ctx, store.Service{Name: "gotenberg", Kind: "http", Enabled: true,
		Params: map[string]string{"url": slow.URL, "t_latency_ms": "20"}})
	svc, _ := s.Store.ServiceByID(ctx, id)

	// no mailer configured: the alert is recorded and shown, nothing is sent
	s.Mail = nil
	st := s.CheckService(ctx, *svc)
	if !st.Up {
		t.Fatalf("slow is not down: %+v", st)
	}
	open, _ := s.Store.OpenAlerts(ctx)
	if len(open) != 1 || open[0].Key != "slow" || open[0].NotifiedAt != nil {
		t.Fatalf("a slow response should open an un-notified alert: %+v", open)
	}
	// email gets configured later: the pending alert is mailed on the next check, once
	s.Mail = mail
	s.CheckService(ctx, *svc)
	s.CheckService(ctx, *svc)
	if got := mail.subjects(); len(got) != 1 || !strings.Contains(got[0], "FIRING: gotenberg (slow)") {
		t.Fatalf("the pending alert should be mailed exactly once: %v", got)
	}
}

func TestConditionsRules(t *testing.T) {
	up := func(m map[string]float64, info map[string]string) Result {
		return Result{Up: true, Metrics: m, Info: info}
	}
	keys := func(cs []Condition) string {
		var k []string
		for _, c := range cs {
			k = append(k, c.Key)
		}
		return strings.Join(k, ",")
	}
	if got := Conditions("postgres", nil, up(map[string]float64{"connections_pct": 90, "connections": 90, "long_queries": 3, "waiting_locks": 1}, map[string]string{"max_connections": "100"}), 0); keys(got) != "connections,long_queries" {
		t.Errorf("postgres: %v", got)
	}
	if got := Conditions("postgres", map[string]string{"t_conn_pct": "95"}, up(map[string]float64{"connections_pct": 90}, nil), 0); len(got) != 0 {
		t.Errorf("a raised threshold must be respected: %v", got)
	}
	if got := Conditions("redis", nil, up(map[string]float64{"memory_pct": 95}, nil), 0); keys(got) != "memory" {
		t.Errorf("redis: %v", got)
	}
	if got := Conditions("redis", nil, up(map[string]float64{"used_memory": 1e9}, nil), 0); len(got) != 0 {
		t.Errorf("no maxmemory means no memory alert: %v", got)
	}
	if got := Conditions("rabbitmq", nil, up(map[string]float64{"alarms": 1, "messages_ready": 20000}, nil), 0); keys(got) != "alarm,backlog" {
		t.Errorf("rabbitmq: %v", got)
	}
	if got := Conditions("mongo", nil, up(map[string]float64{"connections_pct": 85}, nil), 0); keys(got) != "connections" {
		t.Errorf("mongo: %v", got)
	}
	down := Result{Up: false, Err: "connection refused"}
	if got := Conditions("redis", nil, down, 1); len(got) != 0 {
		t.Errorf("one failure is a blip: %v", got)
	}
	if got := Conditions("redis", nil, down, 2); keys(got) != "down" || !strings.Contains(got[0].Message, "connection refused") {
		t.Errorf("two failures are down: %v", got)
	}
	if got := Conditions("redis", map[string]string{"fail_checks": "4"}, down, 3); len(got) != 0 {
		t.Errorf("fail_checks must be respected: %v", got)
	}
}
