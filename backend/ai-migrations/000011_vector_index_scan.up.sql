-- Keep publication checks correlated with each vector. Flattening this into a
-- join can put the distance sort above the join and discard the HNSW ordered
-- scan. OFFSET 0 preserves the EXISTS subplan without changing visibility.
-- CREATE OR REPLACE retains the view's existing query/compute role grants.
CREATE OR REPLACE VIEW ai_published_vectors AS
SELECT v.* FROM ai_chunk_vectors v
WHERE EXISTS (
 SELECT 1 FROM ai_published_sets s WHERE s.id=v.embedding_set_id OFFSET 0
);
