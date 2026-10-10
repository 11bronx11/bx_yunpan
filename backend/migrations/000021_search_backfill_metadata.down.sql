BEGIN;
ALTER TABLE search_projection_checkpoints
    DROP COLUMN IF EXISTS checksum,
    DROP COLUMN IF EXISTS watermark;
ALTER TABLE search_index_versions
    DROP COLUMN IF EXISTS snapshot_cursor;
COMMIT;
