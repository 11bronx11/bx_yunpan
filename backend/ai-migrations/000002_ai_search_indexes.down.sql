DROP INDEX ai_artifacts_age,ai_vectors_cosine,ai_child_parent,ai_child_fts;
DROP TRIGGER ai_child_fts ON ai_child_chunks;
DROP FUNCTION ai_child_fts_update(),ai_fts_query(text),ai_fts_text(text);
