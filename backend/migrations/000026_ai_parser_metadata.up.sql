BEGIN;

ALTER TABLE ai_document_versions
    ADD COLUMN parser_id text NOT NULL DEFAULT '',
    ADD COLUMN parser_version text NOT NULL DEFAULT '',
    ADD COLUMN chunker_version text NOT NULL DEFAULT '',
    ADD COLUMN tokenizer_id text NOT NULL DEFAULT '',
    ADD COLUMN parser_warnings text[] NOT NULL DEFAULT ARRAY[]::text[];

COMMIT;
