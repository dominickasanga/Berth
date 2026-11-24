-- Berth measures its own server ("system") and each app's containers ("container") with the same
-- samples, charts and alerts as the supporting services. They are created and removed by Berth itself.
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_kind_check;
ALTER TABLE services ADD CONSTRAINT services_kind_check
    CHECK (kind IN ('postgres','redis','mongo','rabbitmq','mysql','http','tcp','system','container'));
