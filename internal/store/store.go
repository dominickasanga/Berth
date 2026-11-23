// Package store is Berth's PostgreSQL state: admins, sessions, apps, env, deploys, audit.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	s := &Store{pool}
	return s, s.migrate(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sqlText, _ := migrationFS.ReadFile("migrations/" + name)
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ---- admins & sessions ----

type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	TOTPSecret   []byte
}

func (s *Store) CreateAdmin(ctx context.Context, username, hash string, totpSealed []byte) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO admins (username, password_hash, totp_secret) VALUES ($1,$2,$3)`, username, hash, totpSealed)
	return err
}

func (s *Store) AdminByUsername(ctx context.Context, username string) (*Admin, error) {
	a := &Admin{}
	err := s.Pool.QueryRow(ctx, `SELECT id, username, password_hash, totp_secret FROM admins WHERE username=$1`, username).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.TOTPSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func hashToken(t string) []byte { h := sha256.Sum256([]byte(t)); return h[:] }

// NewSession returns the cookie value; only its hash is stored.
func (s *Store) NewSession(ctx context.Context, adminID int64, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.Pool.Exec(ctx, `INSERT INTO sessions (token_hash, admin_id, expires_at) VALUES ($1,$2,$3)`,
		hashToken(token), adminID, time.Now().Add(ttl))
	return token, err
}

func (s *Store) SessionAdmin(ctx context.Context, token string) (*Admin, error) {
	a := &Admin{}
	err := s.Pool.QueryRow(ctx, `SELECT a.id, a.username, a.password_hash, a.totp_secret
		FROM sessions s JOIN admins a ON a.id = s.admin_id
		WHERE s.token_hash=$1 AND s.expires_at > now()`, hashToken(token)).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.TOTPSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, hashToken(token))
	return err
}

// ---- apps ----

type App struct {
	ID           int64
	Name         string
	RepoURL      string
	Branch       string
	Subfolder    string
	ComposeFile  string
	PredeploySvc string
	SourcePath   string
	Monitor      bool
	SourceMode   string // "clone" (Berth's own clone) or "existing" (the folder in SourcePath)
	// ComposeProject is the compose project name; empty means "berth-<name>".
	ComposeProject string
}

const appCols = `id, name, repo_url, branch, subfolder, compose_file, predeploy_svc, source_path, monitor, source_mode, compose_project`

func scanApp(row pgx.Row) (*App, error) {
	a := &App{}
	err := row.Scan(&a.ID, &a.Name, &a.RepoURL, &a.Branch, &a.Subfolder, &a.ComposeFile, &a.PredeploySvc, &a.SourcePath, &a.Monitor, &a.SourceMode, &a.ComposeProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *Store) CreateApp(ctx context.Context, a App) error {
	if a.SourceMode == "" {
		a.SourceMode = "clone"
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO apps (name, repo_url, branch, subfolder, compose_file, predeploy_svc, source_path, monitor, source_mode, compose_project)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, a.Name, a.RepoURL, a.Branch, a.Subfolder, a.ComposeFile, a.PredeploySvc, a.SourcePath, a.Monitor, a.SourceMode, a.ComposeProject)
	return err
}

func (s *Store) AppByName(ctx context.Context, name string) (*App, error) {
	return scanApp(s.Pool.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE name=$1`, name))
}

func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+appCols+` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpdateAppSource switches how an app runs: "clone" (Berth's own copy) or "existing" (a folder).
func (s *Store) UpdateAppSource(ctx context.Context, id int64, mode, path, repoURL, branch string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE apps SET source_mode=$2, source_path=$3, repo_url=$4, branch=$5 WHERE id=$1`,
		id, mode, path, repoURL, branch)
	return err
}

// ---- discovery ----

func (s *Store) ListScanPaths(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT path FROM scan_paths ORDER BY added_at, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) AddScanPath(ctx context.Context, path string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO scan_paths (path) VALUES ($1) ON CONFLICT DO NOTHING`, path)
	return err
}

func (s *Store) DeleteScanPath(ctx context.Context, path string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM scan_paths WHERE path=$1`, path)
	return err
}

func (s *Store) Choices(ctx context.Context) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT expected, path FROM expected_choice`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SetChoice(ctx context.Context, expected, path string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO expected_choice (expected, path) VALUES ($1,$2)
		ON CONFLICT (expected) DO UPDATE SET path=EXCLUDED.path`, expected, path)
	return err
}

// ---- env ----

type EnvVar struct {
	Key      string
	Value    []byte // encrypted
	IsSecret bool
}

func (s *Store) SetEnv(ctx context.Context, appID int64, key string, sealed []byte, secret bool) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO app_env (app_id, key, value, is_secret) VALUES ($1,$2,$3,$4)
		ON CONFLICT (app_id, key) DO UPDATE SET value=EXCLUDED.value, is_secret=EXCLUDED.is_secret`,
		appID, key, sealed, secret)
	return err
}

func (s *Store) DeleteEnv(ctx context.Context, appID int64, key string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM app_env WHERE app_id=$1 AND key=$2`, appID, key)
	return err
}

func (s *Store) ListEnv(ctx context.Context, appID int64) ([]EnvVar, error) {
	rows, err := s.Pool.Query(ctx, `SELECT key, value, is_secret FROM app_env WHERE app_id=$1 ORDER BY key`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnvVar
	for rows.Next() {
		var e EnvVar
		if err := rows.Scan(&e.Key, &e.Value, &e.IsSecret); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- deploys ----

type Deploy struct {
	ID           int64
	AppID        int64
	Status       string
	CommitSHA    string
	Log          string
	StartedAt    time.Time
	FinishedAt   *time.Time
	Kind         string // "deploy" or "rollback"
	ImagesTagged bool
}

const deployCols = `id, app_id, status, commit_sha, log, started_at, finished_at, kind, images_tagged`

func (s *Store) StartDeploy(ctx context.Context, appID int64, kind string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO deploys (app_id, kind) VALUES ($1,$2) RETURNING id`, appID, kind).Scan(&id)
	return id, err
}

func (s *Store) MarkImagesTagged(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE deploys SET images_tagged=true WHERE id=$1`, id)
	return err
}

func (s *Store) AppendLog(ctx context.Context, id int64, text string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE deploys SET log = log || $2 WHERE id=$1`, id, text)
	return err
}

func (s *Store) FinishDeploy(ctx context.Context, id int64, status, sha string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE deploys SET status=$2, commit_sha=$3, finished_at=now() WHERE id=$1`, id, status, sha)
	return err
}

// UnmarkImagesTagged is called when old image tags are pruned: those deploys can no longer be rolled back to.
func (s *Store) UnmarkImagesTagged(ctx context.Context, ids []int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE deploys SET images_tagged=false WHERE id = ANY($1)`, ids)
	return err
}

// FailStaleDeploys marks deploys left 'running' by a crashed process as failed.
func (s *Store) FailStaleDeploys(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, `UPDATE deploys SET status='failed', finished_at=now(),
		log = log || E'\n[berth restarted during this deploy]\n' WHERE status='running'`)
	return err
}

func (s *Store) LatestDeploys(ctx context.Context, appID int64, n int) ([]Deploy, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+deployCols+`
		FROM deploys WHERE app_id=$1 ORDER BY id DESC LIMIT $2`, appID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deploy
	for rows.Next() {
		var d Deploy
		if err := rows.Scan(&d.ID, &d.AppID, &d.Status, &d.CommitSHA, &d.Log, &d.StartedAt, &d.FinishedAt, &d.Kind, &d.ImagesTagged); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DeployByID(ctx context.Context, id int64) (*Deploy, error) {
	d := &Deploy{}
	err := s.Pool.QueryRow(ctx, `SELECT `+deployCols+` FROM deploys WHERE id=$1`, id).
		Scan(&d.ID, &d.AppID, &d.Status, &d.CommitSHA, &d.Log, &d.StartedAt, &d.FinishedAt, &d.Kind, &d.ImagesTagged)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// ---- audit ----

func (s *Store) Audit(ctx context.Context, who, action, target string) {
	s.Pool.Exec(ctx, `INSERT INTO audit_log (who, action, target) VALUES ($1,$2,$3)`, who, action, target)
	log.Printf("audit: %s %s %s", who, action, target)
}

type AuditRow struct {
	ID                  int64
	At                  time.Time
	Who, Action, Target string
}

// ListAudit returns the newest audit rows first; before > 0 pages back to rows older than that id.
func (s *Store) ListAudit(ctx context.Context, limit int, before int64) ([]AuditRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, at, who, action, target FROM audit_log
		WHERE ($2 = 0 OR id < $2) ORDER BY id DESC LIMIT $1`, limit, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.At, &r.Who, &r.Action, &r.Target); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
