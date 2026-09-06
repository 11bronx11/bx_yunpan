BEGIN;
DROP INDEX IF EXISTS idx_workflow_steps_state;
DROP INDEX IF EXISTS idx_workflow_instances_state;
DROP TABLE IF EXISTS command_deduplications;
DROP TABLE IF EXISTS workflow_steps;
DROP TABLE IF EXISTS workflow_instances;
COMMIT;
