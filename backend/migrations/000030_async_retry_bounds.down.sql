BEGIN;
-- Refuse a lossy rollback while terminal work still needs operator attention.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM outbox_recoveries WHERE stopped_at IS NOT NULL) THEN
        RAISE EXCEPTION 'stopped recovery work requires explicit repair before rollback';
    END IF;
END $$;
-- Even succeeded compensation records are retained for audit and may not be
-- silently discarded when rolling back to code that cannot understand them.
ALTER TABLE storage_operations DROP CONSTRAINT storage_operations_operation_type_check;
ALTER TABLE storage_operations ADD CONSTRAINT storage_operations_operation_type_check
    CHECK (operation_type IN ('multipart.create', 'multipart.complete', 'multipart.abort',
                             'canonical.ensure', 'staging.delete', 'object.delete'));
ALTER TABLE storage_operations DROP CONSTRAINT storage_operations_state_check;
ALTER TABLE storage_operations ADD CONSTRAINT storage_operations_state_check
    CHECK (state IN ('pending', 'running', 'succeeded', 'failed'));
ALTER TABLE outbox_recoveries DROP COLUMN stopped_at;
COMMIT;
