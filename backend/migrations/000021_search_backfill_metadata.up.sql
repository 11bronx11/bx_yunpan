BEGIN;

-- Backfill cursors are opaque UUIDs because snapshot scans are keyset based.
-- The numeric watermark remains useful for operators and event horizons.
ALTER TABLE search_index_versions
    ADD COLUMN IF NOT EXISTS snapshot_cursor uuid;

ALTER TABLE search_projection_checkpoints
    ADD COLUMN IF NOT EXISTS watermark bigint,
    ADD COLUMN IF NOT EXISTS checksum varchar(128);

COMMENT ON COLUMN search_index_versions.snapshot_cursor IS
    'Last committed keyset cursor for the resumable snapshot';
COMMENT ON COLUMN search_projection_checkpoints.watermark IS
    'Optional source/event watermark observed by the projection attempt';

COMMIT;
