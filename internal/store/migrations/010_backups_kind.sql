-- Berth reads the run reports of the TusK backup agents and measures the backups folder as one
-- internal service called "backups" (same samples, charts and alerts as the others).
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_kind_check;
ALTER TABLE services ADD CONSTRAINT services_kind_check
    CHECK (kind IN ('postgres','redis','mongo','rabbitmq','mysql','http','tcp','system','container','backups'));
