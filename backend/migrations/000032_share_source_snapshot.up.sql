BEGIN;

-- Share history retains its own display metadata even after source retirement.
ALTER TABLE shares
    ADD COLUMN file_name varchar(255) NOT NULL DEFAULT '',
    ADD COLUMN mime_type varchar(255) NOT NULL DEFAULT 'application/octet-stream',
    ADD COLUMN source_deleted_at timestamptz;

UPDATE shares s
SET file_name = e.name,
    mime_type = COALESCE(o.mime_type, 'application/octet-stream'),
    source_deleted_at = e.deleted_at
FROM file_entries e LEFT JOIN file_objects o ON o.id = e.object_id
WHERE e.id = s.file_entry_id;

-- file_entry_id remains an immutable source identifier, not a lifetime FK.
ALTER TABLE shares DROP CONSTRAINT shares_file_entry_id_fkey;
CREATE INDEX idx_shares_live_source ON shares(file_entry_id) WHERE source_deleted_at IS NULL;
CREATE INDEX idx_shares_owner_page ON shares(owner_id, created_at DESC, id DESC);

COMMIT;
