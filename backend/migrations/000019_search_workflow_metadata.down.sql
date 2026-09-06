BEGIN;

DROP INDEX IF EXISTS idx_search_index_versions_state;
DROP INDEX IF EXISTS uq_search_index_versions_target;
ALTER TABLE workflow_steps DROP COLUMN IF EXISTS updated_at;
ALTER TABLE search_index_versions
    DROP COLUMN IF EXISTS checksum,
    DROP COLUMN IF EXISTS snapshot_watermark,
    DROP COLUMN IF EXISTS index_epoch;

COMMIT;
