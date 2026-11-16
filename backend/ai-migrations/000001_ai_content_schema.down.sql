-- Refuse accidental data loss. Archive/export AI data explicitly before down.
DO $$ BEGIN IF EXISTS(SELECT 1 FROM ai_objects) THEN RAISE EXCEPTION 'nonempty AI schema requires explicit archive before downgrade'; END IF; END $$;
DROP VIEW ai_published_projections,ai_published_targets,ai_published_capabilities,ai_published_summaries,ai_published_vectors,ai_published_sets,ai_published_children,ai_published_parents,ai_published_content,ai_published_blocks,ai_published_sources,ai_published_objects;
ALTER TABLE ai_objects DROP CONSTRAINT ai_active_source, DROP CONSTRAINT ai_active_content, DROP CONSTRAINT ai_active_vectors, DROP CONSTRAINT ai_active_summary;
DROP TABLE ai_migration_items,ai_migration_runs,ai_projection_states,ai_projection_targets,ai_capability_status,ai_commit_receipts,ai_artifact_manifests,ai_summaries,ai_chunk_vectors,ai_embedding_sets,ai_child_chunks,ai_parent_chunks,ai_content_versions,ai_source_blocks,ai_sources,ai_objects;
