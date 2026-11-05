BEGIN;
ALTER TABLE storage_operations DROP CONSTRAINT storage_operations_operation_type_check;
ALTER TABLE storage_operations ADD CONSTRAINT storage_operations_operation_type_check
    CHECK (operation_type IN ('multipart.create', 'multipart.complete', 'multipart.abort',
                             'canonical.ensure', 'staging.delete', 'object.delete', 'variant.delete'));
ALTER TABLE storage_operations DROP CONSTRAINT storage_operations_state_check;
ALTER TABLE storage_operations ADD CONSTRAINT storage_operations_state_check
    CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'dead'));
ALTER TABLE outbox_recoveries ADD COLUMN stopped_at timestamptz;
COMMENT ON COLUMN outbox_recoveries.stopped_at IS 'Automatic recovery budget exhausted; requires explicit operator repair.';
COMMIT;
