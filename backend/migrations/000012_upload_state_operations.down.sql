BEGIN;

ALTER TABLE upload_sessions
    DROP CONSTRAINT IF EXISTS upload_sessions_status_check;

COMMIT;
