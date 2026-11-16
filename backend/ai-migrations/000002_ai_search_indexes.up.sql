CREATE FUNCTION ai_fts_text(value text) RETURNS tsvector LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT to_tsvector('simple', COALESCE(value,'')) || to_tsvector('simple', COALESCE((SELECT string_agg(substr(value,n,2),' ') FROM generate_series(1,greatest(0,char_length(value)-1)) n WHERE substr(value,n,2) ~ '[一-龥]'),''))
$$;
CREATE FUNCTION ai_child_fts_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.content_tsv=ai_fts_text(NEW.content); RETURN NEW; END $$;
CREATE TRIGGER ai_child_fts BEFORE INSERT OR UPDATE OF content ON ai_child_chunks FOR EACH ROW EXECUTE FUNCTION ai_child_fts_update();
CREATE INDEX ai_child_fts ON ai_child_chunks USING gin(content_tsv);
CREATE INDEX ai_child_parent ON ai_child_chunks(content_version_id,parent_id);
CREATE INDEX ai_vectors_cosine ON ai_chunk_vectors USING hnsw(embedding vector_cosine_ops);
CREATE INDEX ai_artifacts_age ON ai_artifact_manifests(created_at,artifact_id);

CREATE FUNCTION ai_fts_query(value text) RETURNS tsquery LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT COALESCE((SELECT string_agg(quote_literal(x), ' | ')::tsquery FROM unnest(tsvector_to_array(ai_fts_text(value))) x), ''::tsquery)
$$;
