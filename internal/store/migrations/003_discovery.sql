-- Folders Berth scans for expected apps. Defaults are seeded once; admins edit them in the UI.
CREATE TABLE scan_paths (
    path     TEXT PRIMARY KEY,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO scan_paths (path) VALUES ('/var/www'), ('/mnt/blockvolume');

-- Which copy the admin flagged when an expected app was found in more than one place.
CREATE TABLE expected_choice (
    expected TEXT PRIMARY KEY,
    path     TEXT NOT NULL
);

ALTER TABLE apps
    ADD COLUMN source_path TEXT    NOT NULL DEFAULT '',    -- the on-disk copy it was discovered from (reference only)
    ADD COLUMN monitor     BOOLEAN NOT NULL DEFAULT false; -- flagged for monitoring
