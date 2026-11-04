BEGIN;

ALTER TABLE ai_chunks
    DROP COLUMN source_kind;

COMMIT;
