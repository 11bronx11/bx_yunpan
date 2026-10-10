BEGIN;
DROP INDEX IF EXISTS idx_outbox_unconfirmed_execution;
DROP TABLE task_executions;
COMMIT;
