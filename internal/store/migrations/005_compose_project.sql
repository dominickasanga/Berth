-- Compose project name. Empty means "berth-<app>". Set it to an existing stack's project
-- (for example "tusk") so Berth adopts that stack in place: same containers, same named volumes.
ALTER TABLE apps ADD COLUMN compose_project TEXT NOT NULL DEFAULT '';
