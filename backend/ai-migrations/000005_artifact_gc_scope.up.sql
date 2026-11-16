ALTER TABLE ai_artifact_manifests ADD COLUMN object_id uuid, ADD COLUMN content_generation bigint;
CREATE INDEX ai_artifacts_gc_scope ON ai_artifact_manifests(object_id,content_generation,created_at,artifact_id);
-- Backfill retirement conservatively; existing inactive versions receive a full grace window.
UPDATE ai_sources s SET retired_at=clock_timestamp() FROM ai_objects o WHERE s.object_id=o.object_id AND s.content_generation=o.content_generation AND s.id IS DISTINCT FROM o.active_source_id AND s.retired_at IS NULL;
UPDATE ai_content_versions s SET retired_at=clock_timestamp() FROM ai_objects o WHERE s.object_id=o.object_id AND s.content_generation=o.content_generation AND s.id IS DISTINCT FROM o.active_content_version_id AND s.retired_at IS NULL;
UPDATE ai_embedding_sets s SET retired_at=clock_timestamp() FROM ai_objects o WHERE s.object_id=o.object_id AND s.content_generation=o.content_generation AND s.id IS DISTINCT FROM o.active_embedding_set_id AND s.retired_at IS NULL;
UPDATE ai_summaries s SET retired_at=clock_timestamp() FROM ai_objects o WHERE s.object_id=o.object_id AND s.content_generation=o.content_generation AND s.id IS DISTINCT FROM o.active_summary_id AND s.retired_at IS NULL;
