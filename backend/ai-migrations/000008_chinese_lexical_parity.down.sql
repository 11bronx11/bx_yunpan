CREATE OR REPLACE FUNCTION ai_fts_text(value text) RETURNS tsvector LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT to_tsvector('simple', COALESCE(value,'')) || to_tsvector('simple', COALESCE((SELECT string_agg(substr(value,n,2),' ') FROM generate_series(1,greatest(0,char_length(value)-1)) n WHERE substr(value,n,2) ~ '[一-龥]'),''))
$$;
CREATE OR REPLACE FUNCTION ai_fts_query(value text) RETURNS tsquery LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT COALESCE((SELECT string_agg(quote_literal(x), ' | ')::tsquery FROM unnest(tsvector_to_array(ai_fts_text(value))) x), ''::tsquery)
$$;

UPDATE ai_child_chunks SET content_tsv=ai_fts_text(content);
DROP FUNCTION ai_search_terms(text,boolean);
