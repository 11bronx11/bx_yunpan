BEGIN;

-- Keep source text and the legacy simple vector intact. Both old rows and new
-- writes receive the same searchable representation without re-embedding.
-- zh-lexical-v1 is mirrored by internal/textsearch. Han unigrams support
-- one-character queries; overlapping bigrams reject scattered characters.
-- ASCII term keys stop PG/ES native tokenizers from interpreting the terms
-- differently. Long words use built-in sha256(bytea), requiring no extension.
CREATE FUNCTION ai_search_terms(value text, for_query boolean DEFAULT false)
RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE
AS $$
DECLARE
    part text[];
    chars text[];
    word text;
    key text;
    terms text[] := '{}';
    n integer;
    i integer;
BEGIN
    FOR part IN SELECT regexp_matches(lower(normalize(value, NFKC)),
        '([〇㐀-䶿一-鿿豈-﫿𠀀-𲎯]+)|([^〇㐀-䶿一-鿿豈-﫿𠀀-𲎯]+)', 'g')
    LOOP
        IF part[1] IS NULL THEN
            FOR word IN SELECT matches[1] FROM regexp_matches(part[2], '([[:alnum:]]+)', 'g') AS words(matches) LOOP
                IF octet_length(word) > 96 THEN
                    key := 'x' || encode(sha256(convert_to('w' || word, 'UTF8')), 'hex');
                ELSE
                    key := 'w' || encode(convert_to(word, 'UTF8'), 'hex');
                END IF;
                terms := array_append(terms, key);
            END LOOP;
        ELSE
            chars := regexp_split_to_array(part[1], '');
            n := array_length(chars, 1);
            FOR i IN 1..n LOOP
                IF NOT for_query OR n = 1 THEN
                    terms := array_append(terms, 'h' || encode(convert_to(chars[i], 'UTF8'), 'hex'));
                END IF;
                IF i < n THEN
                    terms := array_append(terms, 'h' || encode(convert_to(chars[i] || chars[i + 1], 'UTF8'), 'hex'));
                END IF;
            END LOOP;
        END IF;
    END LOOP;
    RETURN array_to_string(terms, ' ');
END;
$$;

ALTER TABLE ai_chunks ADD COLUMN content_search_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('pg_catalog.simple', ai_search_terms(content))) STORED;

CREATE INDEX idx_ai_chunks_search_fts ON ai_chunks USING gin(content_search_tsv);

COMMIT;
