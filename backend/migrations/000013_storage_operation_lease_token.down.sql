BEGIN;

ALTER TABLE storage_operations
    DROP COLUMN IF EXISTS lease_token;

COMMIT;
