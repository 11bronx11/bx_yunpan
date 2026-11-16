DROP TABLE ai_migration_legacy_tasks;
DROP VIEW ai_published_blocks;
DROP VIEW ai_published_sources;
ALTER TABLE ai_sources DROP COLUMN coverage;
CREATE VIEW ai_published_sources AS SELECT s.* FROM ai_sources s JOIN ai_objects o USING(object_id,content_generation) WHERE o.tombstoned_at IS NULL AND s.publication_state='published';
CREATE VIEW ai_published_blocks AS SELECT b.* FROM ai_source_blocks b JOIN ai_published_sources s ON s.id=b.source_id;
