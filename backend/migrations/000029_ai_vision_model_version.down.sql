BEGIN;

ALTER TABLE ai_chunks
    DROP COLUMN vision_model_version;

ALTER TABLE ai_document_versions
    DROP COLUMN vision_model_version;

COMMIT;
