BEGIN;

ALTER TABLE outbox_deliveries
    DROP CONSTRAINT IF EXISTS outbox_deliveries_status_check;
ALTER TABLE outbox_deliveries
    ADD CONSTRAINT outbox_deliveries_status_check CHECK (status IN ('pending', 'sending', 'published', 'failed'));
ALTER TABLE outbox_deliveries
    ADD COLUMN IF NOT EXISTS lease_until timestamptz,
    ADD COLUMN IF NOT EXISTS lease_token uuid;

CREATE INDEX IF NOT EXISTS idx_upload_sessions_active
    ON upload_sessions(user_id, status, expires_at, created_at DESC)
    WHERE status IN ('initializing', 'created', 'uploading', 'completing', 'verifying');

CREATE INDEX IF NOT EXISTS idx_upload_sessions_name_active
    ON upload_sessions(user_id, folder_id, filename_normalized, expires_at)
    WHERE status IN ('initializing', 'created', 'uploading', 'completing', 'verifying');

CREATE INDEX IF NOT EXISTS idx_storage_operations_ready
    ON storage_operations(operation_type, available_at, created_at)
    WHERE state IN ('pending', 'failed');

CREATE INDEX IF NOT EXISTS idx_storage_operations_lease
    ON storage_operations(lease_until, updated_at)
    WHERE state = 'running';

CREATE INDEX IF NOT EXISTS idx_outbox_deliveries_ready
    ON outbox_deliveries(available_at, event_id)
    WHERE transport = 'asynq' AND status IN ('pending', 'failed');

CREATE INDEX IF NOT EXISTS idx_outbox_deliveries_lease
    ON outbox_deliveries(lease_until, event_id)
    WHERE transport = 'asynq' AND status = 'sending';

CREATE INDEX IF NOT EXISTS idx_file_entries_name_trgm
    ON file_entries USING gin (name_normalized gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_file_objects_gc
    ON file_objects(status, reference_count, updated_at)
    WHERE status IN ('ready', 'deleting');

CREATE INDEX IF NOT EXISTS idx_async_tasks_recovery
    ON async_tasks(status, updated_at, task_type);

COMMIT;
