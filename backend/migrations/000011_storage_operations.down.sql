BEGIN;

DROP INDEX IF EXISTS idx_storage_operations_object;
DROP INDEX IF EXISTS idx_storage_operations_upload;
DROP INDEX IF EXISTS idx_storage_operations_claim;
DROP TABLE IF EXISTS storage_operations;

COMMIT;
