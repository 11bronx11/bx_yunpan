ALTER TABLE queue_budgets ADD COLUMN backpressured bool NOT NULL DEFAULT false;
ALTER TABLE event_inbox ADD COLUMN plan_version int NOT NULL DEFAULT 1;
ALTER TABLE queue_budgets ADD COLUMN recovery_after timestamptz NOT NULL DEFAULT 'epoch';
CREATE INDEX task_sources_by_source ON task_sources(source_key,task_id);
CREATE INDEX tasks_retention ON tasks(updated_at,id) WHERE state IN ('succeeded','superseded','rejected','failed');
CREATE INDEX results_retention ON results(committed_at,id) WHERE committed_at IS NOT NULL;
CREATE INDEX executions_retention ON executions(finished_at,task_id) WHERE finished_at IS NOT NULL;
CREATE INDEX executions_unfinished_outputs ON executions(finished_at,task_id) WHERE outcome IN ('lease_lost','retry_wait','exhausted') AND outputs<>'[]'::jsonb;
CREATE INDEX inbox_retention ON event_inbox(created_at,id) WHERE status='accepted';
