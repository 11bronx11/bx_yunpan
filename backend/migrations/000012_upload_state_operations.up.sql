BEGIN;

ALTER TABLE upload_sessions
    DROP CONSTRAINT IF EXISTS upload_sessions_status_check;
ALTER TABLE upload_sessions
    ADD CONSTRAINT upload_sessions_status_check CHECK (status IN (
        'initializing', 'created', 'uploading', 'completing', 'verifying',
        'completed', 'failed', 'aborted', 'expired'
    ));

ALTER TABLE storage_operations
    ADD COLUMN IF NOT EXISTS storage_upload_id varchar(255);

COMMIT;
