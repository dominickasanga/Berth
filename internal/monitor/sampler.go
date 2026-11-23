package monitor

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/store"
)

// Sampler checks every enabled service on an interval, stores the samples, and raises alerts.
type Sampler struct {
	Store     *store.Store
	Box       *crypt.Box
	Mail      Mailer // nil: alerts are recorded and shown, but no email is sent
	Interval  time.Duration
	Retention time.Duration
	PublicURL string // optional, for links in alert emails

	running sync.Map // service id -> struct{}: never run two checks of one service at once
}

// SpecFor decrypts a stored service into what a collector needs.
func (s *Sampler) SpecFor(v store.Service) (Spec, error) {
	sp := Spec{Name: v.Name, Kind: v.Kind, Host: v.Host, Port: v.Port, Username: v.Username, Params: v.Params}
	if len(v.Password) > 0 {
		pw, err := s.Box.Open(v.Password)
		if err != nil {
			return sp, fmt.Errorf("cannot decrypt the saved password: %w", err)
		}
		sp.Password = pw
	}
	return sp, nil
}

// Run checks all services every Interval until ctx ends, and prunes old samples hourly.
func (s *Sampler) Run(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = 30 * time.Second
	}
	if s.Retention <= 0 {
		s.Retention = 7 * 24 * time.Hour
	}
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	s.CheckAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.CheckAll(ctx)
		case <-prune.C:
			if n, err := s.Store.PruneSamples(ctx, time.Now().Add(-s.Retention)); err != nil {
				log.Printf("monitor: pruning samples failed: %v", err)
			} else if n > 0 {
				log.Printf("monitor: pruned %d old samples", n)
			}
		}
	}
}

func (s *Sampler) CheckAll(ctx context.Context) {
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		log.Printf("monitor: cannot list services: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, v := range services {
		if !v.Enabled {
			continue
		}
		wg.Add(1)
		go func(v store.Service) {
			defer wg.Done()
			s.CheckService(ctx, v)
		}(v)
	}
	wg.Wait()
}

// CheckService runs one check and records it. It returns the new state.
func (s *Sampler) CheckService(ctx context.Context, v store.Service) *store.ServiceState {
	if _, busy := s.running.LoadOrStore(v.ID, struct{}{}); busy {
		return nil
	}
	defer s.running.Delete(v.ID)

	collect := Collector(v.Kind)
	sp, err := s.SpecFor(v)
	now := time.Now()
	prevState, _ := s.Store.ServiceState(ctx, v.ID)
	res := newResult()
	switch {
	case collect == nil:
		res.Err = "unknown kind " + v.Kind
	case err != nil:
		res.Err = err.Error()
	default:
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		prev := Prev{}
		if prevState != nil && prevState.Up {
			prev = Prev{Raw: prevState.Raw, At: prevState.CheckedAt}
		}
		res = collect(cctx, sp, prev, now)
		cancel()
	}

	st := store.ServiceState{ServiceID: v.ID, Up: res.Up, Error: res.Err, Info: res.Info, Latest: res.Metrics, Tables: res.Tables, Raw: res.Raw}
	samples := map[string]float64{"up": 1}
	for k, val := range res.Metrics {
		samples[k] = val
	}
	if res.Up {
		st.Failures = 0
	} else {
		samples = map[string]float64{"up": 0}
		if prevState != nil {
			st.Failures = prevState.Failures + 1
			st.Info, st.Latest, st.Tables = prevState.Info, prevState.Latest, prevState.Tables // keep the last known values
		} else {
			st.Failures = 1
		}
		st.Raw = nil // the next check after recovery has no rate to compute
	}
	if err := s.Store.SaveCheck(ctx, st, samples, now); err != nil {
		log.Printf("monitor: saving the check of %s failed: %v", v.Name, err)
		return nil
	}
	s.reconcileAlerts(ctx, v, Conditions(v.Kind, v.Params, res, st.Failures))
	out, _ := s.Store.ServiceState(ctx, v.ID)
	return out
}

// reconcileAlerts opens alerts for new conditions, resolves those that cleared, and emails each
// transition once. Emails that could not be sent are retried on later checks.
func (s *Sampler) reconcileAlerts(ctx context.Context, v store.Service, conds []Condition) {
	want := map[string]Condition{}
	for _, c := range conds {
		want[c.Key] = c
		_, created, err := s.Store.OpenAlert(ctx, v.ID, c.Key, c.Severity, c.Message)
		if err != nil {
			log.Printf("monitor: opening an alert for %s failed: %v", v.Name, err)
			continue
		}
		if created {
			log.Printf("alert: %s %s %s", v.Name, c.Key, c.Message)
		}
	}
	open, err := s.Store.OpenAlertsFor(ctx, v.ID)
	if err != nil {
		return
	}
	for _, a := range open {
		if _, still := want[a.Key]; !still {
			if err := s.Store.ResolveAlert(ctx, a.ID); err == nil {
				log.Printf("alert resolved: %s %s", v.Name, a.Key)
				s.notify(ctx, v, a, true)
			}
			continue
		}
		if a.NotifiedAt == nil {
			s.notify(ctx, v, a, false)
		}
	}
	// a resolved alert whose "resolved" mail failed earlier
	if recent, err := s.Store.RecentResolvedAlerts(ctx, 20); err == nil {
		for _, a := range recent {
			if a.ServiceID == v.ID && a.NotifiedAt != nil && a.ResolvedNotifiedAt == nil {
				s.notify(ctx, v, a, true)
			}
		}
	}
}

func (s *Sampler) notify(ctx context.Context, v store.Service, a store.Alert, resolved bool) {
	if s.Mail == nil {
		return
	}
	state, what := "FIRING", a.Severity
	if resolved {
		state, what = "RESOLVED", "back to normal"
	}
	subject := fmt.Sprintf("[Berth] %s: %s (%s)", state, v.Name, a.Key)
	problem := a.Message
	if strings.HasPrefix(problem, "is ") { // "is down: ..." reads as a sentence with the name in front
		problem = v.Name + " " + problem
	}
	body := fmt.Sprintf("%s\n\nService: %s (%s %s:%d)\nStatus: %s\nOpened: %s\n", problem, v.Name, v.Kind, v.Host, v.Port, what, a.OpenedAt.Format(time.RFC1123))
	if s.PublicURL != "" {
		body += fmt.Sprintf("\nOpen in Berth: %s/services/%d\n", s.PublicURL, v.ID)
	}
	if err := s.Mail.Send(subject, body); err != nil {
		log.Printf("monitor: sending the alert email failed: %v", err)
		return
	}
	s.Store.MarkAlertNotified(ctx, a.ID, resolved)
}
