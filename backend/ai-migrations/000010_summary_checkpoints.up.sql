-- Private immutable map/reduce nodes. Compute may read only these checkpoints,
-- never arbitrary staging; the existing artifact GC retains control authority.
CREATE VIEW ai_summary_checkpoints AS
SELECT * FROM ai_artifact_manifests
WHERE kind LIKE 'ai.summary.checkpoint.v1:%';
