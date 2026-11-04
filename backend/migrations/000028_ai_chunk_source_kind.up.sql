BEGIN;

ALTER TABLE ai_chunks
    ADD COLUMN source_kind varchar(32) NOT NULL DEFAULT 'native';

COMMIT;
