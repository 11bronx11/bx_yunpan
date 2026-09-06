BEGIN;

-- Search version control is additive. Existing rows remain valid with a
-- default epoch and no snapshot watermark/checksum until the next rebuild.
ALTER TABLE search_index_versions
    ADD COLUMN IF NOT EXISTS index_epoch bigint NOT NULL DEFAULT 1
        CHECK (index_epoch > 0),
    ADD COLUMN IF NOT EXISTS snapshot_watermark bigint,
    ADD COLUMN IF NOT EXISTS checksum varchar(128);

-- Updated-at is needed by the durable workflow state machine when an
-- operator reconciles timed-out steps without relying on process memory.
ALTER TABLE workflow_steps
    ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS idx_search_index_versions_state
    ON search_index_versions(backend, projection_type, state, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_search_index_versions_target
    ON search_index_versions(backend, projection_type, target_version);

COMMIT;
