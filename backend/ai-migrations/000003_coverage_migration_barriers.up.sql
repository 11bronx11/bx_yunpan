ALTER TABLE ai_sources ADD COLUMN coverage text NOT NULL DEFAULT 'complete' CHECK(coverage IN ('complete','partial','none'));
CREATE OR REPLACE VIEW ai_published_sources AS SELECT s.* FROM ai_sources s JOIN ai_objects o USING(object_id,content_generation) WHERE o.tombstoned_at IS NULL AND s.publication_state='published';
CREATE TABLE ai_migration_legacy_tasks (
 run_id uuid NOT NULL REFERENCES ai_migration_runs(id), legacy_task_id uuid NOT NULL,
 object_id uuid NOT NULL, content_generation bigint NOT NULL,
 state text NOT NULL, attempts integer NOT NULL, dispatches integer NOT NULL, recoveries integer NOT NULL,
 max_attempts integer NOT NULL, max_dispatches integer NOT NULL, max_recoveries integer NOT NULL,
 deadline timestamptz NOT NULL, result_id uuid, input_digest text NOT NULL,
 PRIMARY KEY(run_id,legacy_task_id)
);
