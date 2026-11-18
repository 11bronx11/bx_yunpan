CREATE OR REPLACE VIEW ai_published_vectors AS
SELECT v.* FROM ai_chunk_vectors v JOIN ai_published_sets s ON s.id=v.embedding_set_id;
