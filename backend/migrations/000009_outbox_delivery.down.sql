BEGIN;

DROP INDEX IF EXISTS idx_outbox_deliveries_poll;
DROP TABLE IF EXISTS outbox_deliveries;
ALTER TABLE outbox_events
    DROP CONSTRAINT IF EXISTS outbox_events_message_kind_check,
    DROP CONSTRAINT IF EXISTS outbox_events_aggregate_version_check,
    DROP COLUMN IF EXISTS message_kind,
    DROP COLUMN IF EXISTS aggregate_version,
    DROP COLUMN IF EXISTS producer,
    DROP COLUMN IF EXISTS occurred_at;

COMMIT;
