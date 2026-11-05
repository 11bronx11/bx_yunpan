BEGIN;

DROP INDEX IF EXISTS idx_ai_chunks_search_fts;
ALTER TABLE ai_chunks DROP COLUMN IF EXISTS content_search_tsv;
DROP FUNCTION IF EXISTS ai_search_terms(text, boolean);

COMMIT;
