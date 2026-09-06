BEGIN;

-- Contract rollback is only valid after version rows/chunks have been drained
-- in an isolated database. Production rollback uses the compatibility reader.
DROP INDEX IF EXISTS idx_ai_document_versions_lease;
DROP INDEX IF EXISTS idx_ai_document_versions_active;
DROP INDEX IF EXISTS idx_ai_chunks_version_ordinal;
ALTER TABLE ai_chunks DROP CONSTRAINT IF EXISTS ai_chunks_document_version_fk;
ALTER TABLE ai_documents DROP CONSTRAINT IF EXISTS ai_documents_active_version_fk;
ALTER TABLE ai_chunks DROP COLUMN IF EXISTS document_version_id;
ALTER TABLE ai_documents DROP COLUMN IF EXISTS active_version_id;
ALTER TABLE ai_chunks ADD CONSTRAINT ai_chunks_document_id_ordinal_key UNIQUE (document_id, ordinal);
DROP TABLE IF EXISTS ai_document_versions;

COMMIT;
