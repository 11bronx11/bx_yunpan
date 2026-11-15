DROP INDEX inbox_retention;
ALTER TABLE event_inbox DROP COLUMN plan_version;
DROP INDEX executions_unfinished_outputs;
DROP INDEX executions_retention;
DROP INDEX results_retention;
DROP INDEX tasks_retention;
DROP INDEX task_sources_by_source;
ALTER TABLE queue_budgets DROP COLUMN recovery_after;
ALTER TABLE queue_budgets DROP COLUMN backpressured;
