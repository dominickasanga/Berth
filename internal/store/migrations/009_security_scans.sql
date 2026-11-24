-- The latest security scan of each app (vulnerable packages, risky containers, secrets in the repository), as JSON.
CREATE TABLE security_scans (
    app        TEXT PRIMARY KEY,
    result     JSONB NOT NULL,
    scanned_at TIMESTAMPTZ NOT NULL
);
