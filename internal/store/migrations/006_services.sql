-- Supporting services Berth monitors (PostgreSQL, Redis, MongoDB, RabbitMQ, MySQL, HTTP, TCP).
CREATE TABLE services (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,40}$'),
    kind       TEXT NOT NULL CHECK (kind IN ('postgres','redis','mongo','rabbitmq','mysql','http','tcp')),
    host       TEXT NOT NULL DEFAULT '',
    port       INTEGER NOT NULL DEFAULT 0 CHECK (port BETWEEN 0 AND 65535),
    username   TEXT NOT NULL DEFAULT '',
    password   BYTEA,                                  -- encrypted, never shown again
    params     JSONB NOT NULL DEFAULT '{}',            -- kind-specific settings and alert thresholds
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per metric per check, kept for BERTH_METRICS_RETENTION_DAYS (default 7).
CREATE TABLE metric_samples (
    service_id BIGINT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    ts         TIMESTAMPTZ NOT NULL,
    metric     TEXT NOT NULL,
    value      DOUBLE PRECISION NOT NULL
);
CREATE INDEX metric_samples_lookup ON metric_samples (service_id, metric, ts);
CREATE INDEX metric_samples_ts ON metric_samples (ts);

-- The latest check of each service: what the status cards show, and the counters needed for rates.
CREATE TABLE service_state (
    service_id  BIGINT PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
    up          BOOLEAN NOT NULL,
    since       TIMESTAMPTZ NOT NULL,                  -- when it last changed between up and down
    checked_at  TIMESTAMPTZ NOT NULL,
    failures    INTEGER NOT NULL DEFAULT 0,            -- consecutive failed checks
    error       TEXT NOT NULL DEFAULT '',
    info        JSONB NOT NULL DEFAULT '{}',
    latest      JSONB NOT NULL DEFAULT '{}',
    tables      JSONB NOT NULL DEFAULT '[]',
    raw         JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE alerts (
    id                   BIGSERIAL PRIMARY KEY,
    service_id           BIGINT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    key                  TEXT NOT NULL,                -- e.g. "down", "connections"
    severity             TEXT NOT NULL,
    message              TEXT NOT NULL,
    opened_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at          TIMESTAMPTZ,
    notified_at          TIMESTAMPTZ,                  -- the "firing" email was sent
    resolved_notified_at TIMESTAMPTZ                   -- the "resolved" email was sent
);
CREATE UNIQUE INDEX alerts_one_open ON alerts (service_id, key) WHERE resolved_at IS NULL;
