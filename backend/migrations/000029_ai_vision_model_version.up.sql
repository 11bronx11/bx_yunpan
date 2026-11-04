BEGIN;

ALTER TABLE ai_document_versions
    ADD COLUMN vision_model_version text NOT NULL DEFAULT '';

ALTER TABLE ai_chunks
    ADD COLUMN vision_model_version text NOT NULL DEFAULT '';

COMMIT;
