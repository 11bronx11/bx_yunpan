-- Refuse rollback while sweep intents remain; do not discard cleanup work.
ALTER TABLE storage_operations DROP CONSTRAINT storage_operations_operation_type_check;
ALTER TABLE storage_operations ADD CONSTRAINT storage_operations_operation_type_check
 CHECK(operation_type IN ('multipart.create','multipart.complete','multipart.abort','canonical.ensure','staging.delete','object.delete','variant.delete'));
