CREATE TABLE admins (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    totp_secret   BYTEA NOT NULL,           -- encrypted
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash BYTEA PRIMARY KEY,           -- sha256 of the cookie value
    admin_id   BIGINT NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE apps (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,40}$'),
    repo_url       TEXT NOT NULL,
    branch         TEXT NOT NULL DEFAULT 'main',
    subfolder      TEXT NOT NULL DEFAULT '',
    compose_file   TEXT NOT NULL DEFAULT 'docker-compose.yml',
    predeploy_svc  TEXT NOT NULL DEFAULT '',  -- compose service run once before start; failure aborts the deploy
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE app_env (
    app_id    BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    key       TEXT NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]*$'),
    value     BYTEA NOT NULL,               -- encrypted
    is_secret BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (app_id, key)
);

CREATE TABLE deploys (
    id          BIGSERIAL PRIMARY KEY,
    app_id      BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    status      TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','success','failed')),
    commit_sha  TEXT NOT NULL DEFAULT '',
    log         TEXT NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE TABLE audit_log (
    id      BIGSERIAL PRIMARY KEY,
    at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    who     TEXT NOT NULL,
    action  TEXT NOT NULL,
    target  TEXT NOT NULL DEFAULT ''
);
