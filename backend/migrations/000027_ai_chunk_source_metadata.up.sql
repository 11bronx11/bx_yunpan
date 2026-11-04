BEGIN;

ALTER TABLE ai_chunks
    ADD COLUMN kind varchar(32) NOT NULL DEFAULT '',
    ADD COLUMN heading_path text NOT NULL DEFAULT '',
    ADD COLUMN page_start integer NOT NULL DEFAULT 0 CHECK (page_start >= 0),
    ADD COLUMN page_end integer NOT NULL DEFAULT 0 CHECK (page_end >= 0),
    ADD COLUMN slide_number integer NOT NULL DEFAULT 0 CHECK (slide_number >= 0),
    ADD COLUMN sheet_name text NOT NULL DEFAULT '',
    ADD COLUMN row_start integer NOT NULL DEFAULT 0 CHECK (row_start >= 0),
    ADD COLUMN row_end integer NOT NULL DEFAULT 0 CHECK (row_end >= 0),
    ADD COLUMN column_start integer NOT NULL DEFAULT 0 CHECK (column_start >= 0),
    ADD COLUMN column_end integer NOT NULL DEFAULT 0 CHECK (column_end >= 0),
    ADD COLUMN source_start bigint NOT NULL DEFAULT 0 CHECK (source_start >= 0),
    ADD COLUMN source_end bigint NOT NULL DEFAULT 0 CHECK (source_end >= 0),
    ADD COLUMN merged_ranges text[] NOT NULL DEFAULT '{}',
    ADD COLUMN encoding varchar(64) NOT NULL DEFAULT '',
    ADD COLUMN estimated_token_count integer NOT NULL DEFAULT 0 CHECK (estimated_token_count >= 0),
    ADD COLUMN token_count_is_estimate boolean NOT NULL DEFAULT false,
    ADD COLUMN parser_id text NOT NULL DEFAULT '',
    ADD COLUMN parser_version text NOT NULL DEFAULT '',
    ADD COLUMN chunker_version text NOT NULL DEFAULT '',
    ADD COLUMN tokenizer_id text NOT NULL DEFAULT '';

COMMIT;
