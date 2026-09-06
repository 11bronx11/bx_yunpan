BEGIN;

ALTER TABLE storage_operations
    ADD COLUMN IF NOT EXISTS lease_token uuid;

COMMIT;
