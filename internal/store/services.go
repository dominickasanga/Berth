package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Service is a supporting service Berth monitors.
type Service struct {
	ID       int64
	Name     string
	Kind     string // postgres, redis, mongo, rabbitmq, mysql, http, tcp
	Host     string
	Port     int
	Username string
	Password []byte // encrypted; nil when none
	Params   map[string]string
	Enabled  bool
}

const serviceCols = `id, name, kind, host, port, username, password, params, enabled`

func scanService(row pgx.Row) (*Service, error) {
	s := &Service{}
	var params []byte
	err := row.Scan(&s.ID, &s.Name, &s.Kind, &s.Host, &s.Port, &s.Username, &s.Password, &params, &s.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Params = map[string]string{}
	json.Unmarshal(params, &s.Params)
	return s, nil
}

func (s *Store) CreateService(ctx context.Context, v Service) (int64, error) {
	params, _ := json.Marshal(v.Params)
	var id int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO services (name, kind, host, port, username, password, params, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		v.Name, v.Kind, v.Host, v.Port, v.Username, v.Password, params, v.Enabled).Scan(&id)
	return id, err
}

// UpdateService changes everything but the name and kind; a nil Password keeps the stored one.
func (s *Store) UpdateService(ctx context.Context, v Service) error {
	params, _ := json.Marshal(v.Params)
	_, err := s.Pool.Exec(ctx, `UPDATE services SET host=$2, port=$3, username=$4,
		password = COALESCE($5, password), params=$6, enabled=$7 WHERE id=$1`,
		v.ID, v.Host, v.Port, v.Username, v.Password, params, v.Enabled)
	return err
}

func (s *Store) DeleteService(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM services WHERE id=$1`, id)
	return err
}

func (s *Store) ServiceByID(ctx context.Context, id int64) (*Service, error) {
	return scanService(s.Pool.QueryRow(ctx, `SELECT `+serviceCols+` FROM services WHERE id=$1`, id))
}

func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+serviceCols+` FROM services ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Service
	for rows.Next() {
		v, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// ServiceState is the latest check of a service.
type ServiceState struct {
	ServiceID int64
	Up        bool
	Since     time.Time
	CheckedAt time.Time
	Failures  int
	Error     string
	Info      map[string]string
	Latest    map[string]float64
	Tables    []Table
	Raw       map[string]float64
}

// Table is a small table of facts a collector found (databases, queues, long queries...).
type Table struct {
	Title   string
	Columns []string
	Rows    [][]string
}

func (s *Store) ServiceState(ctx context.Context, id int64) (*ServiceState, error) {
	st := &ServiceState{ServiceID: id}
	var info, latest, tables, raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT up, since, checked_at, failures, error, info, latest, tables, raw
		FROM service_state WHERE service_id=$1`, id).Scan(&st.Up, &st.Since, &st.CheckedAt, &st.Failures, &st.Error, &info, &latest, &tables, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal(info, &st.Info)
	json.Unmarshal(latest, &st.Latest)
	json.Unmarshal(tables, &st.Tables)
	json.Unmarshal(raw, &st.Raw)
	return st, nil
}

// SaveCheck records one check: the new state (keeping "since" unless up/down changed) and its samples.
func (s *Store) SaveCheck(ctx context.Context, st ServiceState, samples map[string]float64, at time.Time) error {
	info, _ := json.Marshal(st.Info)
	latest, _ := json.Marshal(st.Latest)
	tables, _ := json.Marshal(st.Tables)
	raw, _ := json.Marshal(st.Raw)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO service_state (service_id, up, since, checked_at, failures, error, info, latest, tables, raw)
		VALUES ($1,$2,$3,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (service_id) DO UPDATE SET
			since = CASE WHEN service_state.up = EXCLUDED.up THEN service_state.since ELSE EXCLUDED.since END,
			up = EXCLUDED.up, checked_at = EXCLUDED.checked_at, failures = EXCLUDED.failures, error = EXCLUDED.error,
			info = EXCLUDED.info, latest = EXCLUDED.latest, tables = EXCLUDED.tables, raw = EXCLUDED.raw`,
		st.ServiceID, st.Up, at, st.Failures, st.Error, info, latest, tables, raw); err != nil {
		return err
	}
	if len(samples) > 0 {
		batch := &pgx.Batch{}
		for metric, v := range samples {
			batch.Queue(`INSERT INTO metric_samples (service_id, ts, metric, value) VALUES ($1,$2,$3,$4)`, st.ServiceID, at, metric, v)
		}
		br := tx.SendBatch(ctx, batch)
		for range samples {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return err
			}
		}
		br.Close()
	}
	return tx.Commit(ctx)
}

// Point is one chart point.
type Point struct {
	T time.Time
	V float64
}

// Series returns a metric over [since, now], averaged into buckets of bucketSecs seconds.
func (s *Store) Series(ctx context.Context, serviceID int64, metric string, since time.Time, bucketSecs int) ([]Point, error) {
	rows, err := s.Pool.Query(ctx, `SELECT to_timestamp(floor(extract(epoch FROM ts) / $4) * $4) AS t, avg(value)
		FROM metric_samples WHERE service_id=$1 AND metric=$2 AND ts >= $3 GROUP BY 1 ORDER BY 1`,
		serviceID, metric, since, bucketSecs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.T, &p.V); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PruneSamples(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM metric_samples WHERE ts < $1`, olderThan)
	return tag.RowsAffected(), err
}

// ---- alerts ----

type Alert struct {
	ID                 int64
	ServiceID          int64
	ServiceName        string
	Key, Severity      string
	Message            string
	OpenedAt           time.Time
	ResolvedAt         *time.Time
	NotifiedAt         *time.Time
	ResolvedNotifiedAt *time.Time
}

// OpenAlert opens an alert; created is false if one with that key is already open.
func (s *Store) OpenAlert(ctx context.Context, serviceID int64, key, severity, message string) (id int64, created bool, err error) {
	err = s.Pool.QueryRow(ctx, `INSERT INTO alerts (service_id, key, severity, message) VALUES ($1,$2,$3,$4)
		ON CONFLICT (service_id, key) WHERE resolved_at IS NULL DO UPDATE SET message = EXCLUDED.message
		RETURNING id, (xmax = 0)`, serviceID, key, severity, message).Scan(&id, &created)
	return
}

func (s *Store) ResolveAlert(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE alerts SET resolved_at = now() WHERE id=$1 AND resolved_at IS NULL`, id)
	return err
}

func (s *Store) MarkAlertNotified(ctx context.Context, id int64, resolved bool) error {
	col := "notified_at"
	if resolved {
		col = "resolved_notified_at"
	}
	_, err := s.Pool.Exec(ctx, `UPDATE alerts SET `+col+` = now() WHERE id=$1`, id)
	return err
}

const alertCols = `a.id, a.service_id, s.name, a.key, a.severity, a.message, a.opened_at, a.resolved_at, a.notified_at, a.resolved_notified_at`

func (s *Store) queryAlerts(ctx context.Context, where string, args ...any) ([]Alert, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+alertCols+` FROM alerts a JOIN services s ON s.id = a.service_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.ServiceID, &a.ServiceName, &a.Key, &a.Severity, &a.Message, &a.OpenedAt, &a.ResolvedAt, &a.NotifiedAt, &a.ResolvedNotifiedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) OpenAlerts(ctx context.Context) ([]Alert, error) {
	return s.queryAlerts(ctx, `WHERE a.resolved_at IS NULL ORDER BY a.opened_at DESC`)
}

func (s *Store) OpenAlertsFor(ctx context.Context, serviceID int64) ([]Alert, error) {
	return s.queryAlerts(ctx, `WHERE a.resolved_at IS NULL AND a.service_id = $1 ORDER BY a.opened_at DESC`, serviceID)
}

func (s *Store) RecentResolvedAlerts(ctx context.Context, limit int) ([]Alert, error) {
	return s.queryAlerts(ctx, `WHERE a.resolved_at IS NOT NULL ORDER BY a.resolved_at DESC LIMIT $1`, limit)
}
