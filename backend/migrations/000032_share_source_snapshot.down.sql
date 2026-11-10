BEGIN;

DROP INDEX idx_shares_owner_page;
DROP INDEX idx_shares_live_source;
ALTER TABLE shares ADD CONSTRAINT shares_file_entry_id_fkey
    FOREIGN KEY (file_entry_id) REFERENCES file_entries(id);
ALTER TABLE shares DROP COLUMN source_deleted_at, DROP COLUMN mime_type, DROP COLUMN file_name;

COMMIT;
