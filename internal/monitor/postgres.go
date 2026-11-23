package monitor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dominickasanga/berth/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func collectPostgres(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s connect_timeout=5",
		sp.Host, sp.Port, sp.Username, Param(sp.Params, "database", "postgres"), Param(sp.Params, "sslmode", "prefer")))
	if err != nil {
		return fail(err)
	}
	cfg.Password = sp.Password
	cfg.RuntimeParams["application_name"] = "berth-monitor"
	cfg.RuntimeParams["statement_timeout"] = "10000"
	start := time.Now()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "28P01" { // invalid_password
			return fail(fmt.Errorf("the server refused the password for user %q (%v). Check the role's password with psql, and re-enter it here: if you changed the username, the saved password belongs to the old one", sp.Username, pgErr.Message))
		}
		return fail(err)
	}
	defer conn.Close(ctx)
	if err := conn.Ping(ctx); err != nil {
		return fail(err)
	}
	r := newResult()
	r.Up = true
	r.Metrics["latency_ms"] = millis(time.Since(start))

	var version string
	var uptime, maxConn float64
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version'), extract(epoch FROM now() - pg_postmaster_start_time()),
		current_setting('max_connections')::float8`).Scan(&version, &uptime, &maxConn); err != nil {
		return fail(err)
	}
	r.Info["version"] = version
	r.Info["uptime"] = humanDuration(uptime)

	var conns, waiting float64
	conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'`).Scan(&conns)
	conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting)
	r.Metrics["connections"] = conns
	r.Metrics["waiting_locks"] = waiting
	if maxConn > 0 {
		r.Metrics["connections_pct"] = 100 * conns / maxConn
		r.Info["max_connections"] = strconv.Itoa(int(maxConn))
	}

	var xacts, deadlocks, hit, read float64
	if err := conn.QueryRow(ctx, `SELECT coalesce(sum(xact_commit + xact_rollback),0)::float8, coalesce(sum(deadlocks),0)::float8,
		coalesce(sum(blks_hit),0)::float8, coalesce(sum(blks_read),0)::float8 FROM pg_stat_database`).Scan(&xacts, &deadlocks, &hit, &read); err == nil {
		r.Raw["xacts"], r.Raw["deadlocks"], r.Raw["blks_hit"], r.Raw["blks_read"] = xacts, deadlocks, hit, read
		if v, ok := rate(prev, r.Raw, "xacts", now); ok {
			r.Metrics["tps"] = v
		}
		if v, ok := rate(prev, r.Raw, "deadlocks", now); ok {
			r.Metrics["deadlocks_per_min"] = v * 60
		}
		dh, ok1 := delta(prev, r.Raw, "blks_hit")
		dr, _ := delta(prev, r.Raw, "blks_read")
		switch {
		case ok1 && dh+dr > 0:
			r.Metrics["cache_hit_pct"] = 100 * dh / (dh + dr)
		case ok1: // nothing read in this interval
		case hit+read > 0:
			r.Metrics["cache_hit_pct"] = 100 * hit / (hit + read) // first check: lifetime ratio
		}
	}

	// databases and their sizes
	dbs := store.Table{Title: "Databases", Columns: []string{"Database", "Size", "Connections"}}
	if rows, err := conn.Query(ctx, `SELECT datname, pg_database_size(datname)::float8, numbackends FROM pg_stat_database
		WHERE datname IS NOT NULL AND datname NOT IN ('template0','template1') ORDER BY 2 DESC LIMIT 25`); err == nil {
		total := 0.0
		for rows.Next() {
			var name string
			var size float64
			var n int
			if rows.Scan(&name, &size, &n) == nil {
				total += size
				dbs.Rows = append(dbs.Rows, []string{name, humanBytes(size), strconv.Itoa(n)})
			}
		}
		rows.Close()
		r.Metrics["db_size_bytes"] = total
		if len(dbs.Rows) > 0 {
			r.Tables = append(r.Tables, dbs)
		}
	}

	// who holds the connections: grouped by application, with numbered names such as
	// "pgAdmin 4 - CONN:4584859" collapsed into one line
	apps := store.Table{Title: "Connections by application", Columns: []string{"Application", "Database", "User", "State", "Connections"}}
	var idle, idleTx float64
	if rows, err := conn.Query(ctx, `SELECT CASE WHEN application_name = '' THEN '(no name)' ELSE regexp_replace(application_name, '[0-9]{3,}', '#', 'g') END,
		coalesce(datname, ''), coalesce(usename, ''), coalesce(state, 'unknown'), count(*)
		FROM pg_stat_activity WHERE backend_type = 'client backend' GROUP BY 1, 2, 3, 4 ORDER BY 5 DESC, 1 LIMIT 20`); err == nil {
		for rows.Next() {
			var app, db, user, state string
			var n int
			if rows.Scan(&app, &db, &user, &state, &n) == nil {
				apps.Rows = append(apps.Rows, []string{app, db, user, state, strconv.Itoa(n)})
			}
		}
		rows.Close()
	}
	conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'idle'), count(*) FILTER (WHERE state LIKE 'idle in transaction%')
		FROM pg_stat_activity WHERE backend_type = 'client backend'`).Scan(&idle, &idleTx)
	r.Metrics["connections_idle"], r.Metrics["idle_in_transaction"] = idle, idleTx
	if len(apps.Rows) > 0 {
		r.Tables = append(r.Tables, apps)
	}

	// long-running queries (query text is only visible with pg_monitor or superuser)
	longSecs := 30
	if n, err := strconv.Atoi(Param(sp.Params, "long_query_secs", "30")); err == nil && n > 0 {
		longSecs = n
	}
	lq := store.Table{Title: fmt.Sprintf("Queries running longer than %ds", longSecs), Columns: []string{"PID", "User", "Database", "State", "Seconds", "Query"}}
	if rows, err := conn.Query(ctx, `SELECT pid, coalesce(usename,''), coalesce(datname,''), coalesce(state,''),
		extract(epoch FROM now() - query_start)::int, left(regexp_replace(coalesce(query,''), '\s+', ' ', 'g'), 120)
		FROM pg_stat_activity WHERE backend_type = 'client backend' AND state <> 'idle' AND pid <> pg_backend_pid()
		AND now() - query_start > make_interval(secs => $1) ORDER BY query_start LIMIT 10`, longSecs); err == nil {
		for rows.Next() {
			var pid, secs int
			var user, db, state, q string
			if rows.Scan(&pid, &user, &db, &state, &secs, &q) == nil {
				lq.Rows = append(lq.Rows, []string{strconv.Itoa(pid), user, db, state, strconv.Itoa(secs), q})
			}
		}
		rows.Close()
	}
	r.Metrics["long_queries"] = float64(len(lq.Rows))
	if len(lq.Rows) > 0 {
		r.Tables = append(r.Tables, lq)
	}

	// schema sizes (opt-in: can be slow on big databases), e.g. Kite360 tenant schemas
	if strings.EqualFold(Param(sp.Params, "schema_sizes", "no"), "yes") {
		sc := store.Table{Title: "Schemas in " + Param(sp.Params, "database", "postgres"), Columns: []string{"Schema", "Size", "Tables"}}
		if rows, err := conn.Query(ctx, `SELECT n.nspname, sum(pg_total_relation_size(c.oid))::float8, count(*) FILTER (WHERE c.relkind = 'r')
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r','m','t') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
			GROUP BY 1 ORDER BY 2 DESC LIMIT 30`); err == nil {
			for rows.Next() {
				var name string
				var size float64
				var tables int
				if rows.Scan(&name, &size, &tables) == nil {
					sc.Rows = append(sc.Rows, []string{name, humanBytes(size), strconv.Itoa(tables)})
				}
			}
			rows.Close()
		}
		r.Tables = append(r.Tables, sc) // shown even when empty, so "no schemas" is visible
	}
	return r
}
