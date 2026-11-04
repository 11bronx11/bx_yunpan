BEGIN;

ALTER TABLE ai_document_versions
    DROP COLUMN parser_warnings,
    DROP COLUMN tokenizer_id,
    DROP COLUMN chunker_version,
    DROP COLUMN parser_version,
    DROP COLUMN parser_id;

COMMIT;
