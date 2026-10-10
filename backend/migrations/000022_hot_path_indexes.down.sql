BEGIN;

DROP INDEX IF EXISTS idx_async_tasks_recovery;
DROP INDEX IF EXISTS idx_file_objects_gc;
DROP INDEX IF EXISTS idx_file_entries_name_trgm;
DROP INDEX IF EXISTS idx_outbox_deliveries_lease;
DROP INDEX IF EXISTS idx_outbox_deliveries_ready;
DROP INDEX IF EXISTS idx_storage_operations_lease;
DROP INDEX IF EXISTS idx_storage_operations_ready;
DROP INDEX IF EXISTS idx_upload_sessions_name_active;
DROP INDEX IF EXISTS idx_upload_sessions_active;

ALTER TABLE outbox_deliveries
    DROP CONSTRAINT IF EXISTS outbox_deliveries_status_check;
ALTER TABLE outbox_deliveries
    ADD CONSTRAINT outbox_deliveries_status_check CHECK (status IN ('pending', 'published', 'failed'));
ALTER TABLE outbox_deliveries
    DROP COLUMN IF EXISTS lease_until,
    DROP COLUMN IF EXISTS lease_token;

COMMIT;
