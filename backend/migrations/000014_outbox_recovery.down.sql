BEGIN;

DROP INDEX IF EXISTS idx_outbox_recoveries_resource;
DROP TABLE IF EXISTS outbox_recoveries;

COMMIT;
