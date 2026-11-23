ALTER TABLE deploys
    ADD COLUMN kind          TEXT    NOT NULL DEFAULT 'deploy' CHECK (kind IN ('deploy','rollback')),
    ADD COLUMN images_tagged BOOLEAN NOT NULL DEFAULT false;  -- images kept as <repo>:berth-<id>, so this deploy can be rolled back to
