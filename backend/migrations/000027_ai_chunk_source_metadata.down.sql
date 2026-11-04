BEGIN;

ALTER TABLE ai_chunks
    DROP COLUMN tokenizer_id,
    DROP COLUMN chunker_version,
    DROP COLUMN parser_version,
    DROP COLUMN parser_id,
    DROP COLUMN token_count_is_estimate,
    DROP COLUMN estimated_token_count,
    DROP COLUMN encoding,
    DROP COLUMN merged_ranges,
    DROP COLUMN source_end,
    DROP COLUMN source_start,
    DROP COLUMN column_end,
    DROP COLUMN column_start,
    DROP COLUMN row_end,
    DROP COLUMN row_start,
    DROP COLUMN sheet_name,
    DROP COLUMN slide_number,
    DROP COLUMN page_end,
    DROP COLUMN page_start,
    DROP COLUMN heading_path,
    DROP COLUMN kind;

COMMIT;
