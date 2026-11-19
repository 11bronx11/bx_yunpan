-- Explicit contract migration, never run by ordinary `migrate control up`.
DO $$ BEGIN
 IF (SELECT value FROM control_settings WHERE key='coordination_protocol') IS DISTINCT FROM 'v3' THEN
  RAISE EXCEPTION 'activate v3 before archiving';
 END IF;
 IF EXISTS(SELECT 1 FROM tasks WHERE protocol_version<>3) OR
    EXISTS(SELECT 1 FROM tasks WHERE state='running' AND lease_until>clock_timestamp()) OR
    EXISTS(SELECT 1 FROM module_leases WHERE lease_until>clock_timestamp()) THEN
  RAISE EXCEPTION 'legacy tasks or live writers remain';
 END IF;
 IF EXISTS(SELECT 1 FROM results WHERE committed_at IS NULL) OR
    EXISTS(SELECT 1 FROM task_continuations WHERE state='blocked') OR
    EXISTS(SELECT 1 FROM executions WHERE outputs<>'[]'::jsonb AND outcome NOT IN ('succeeded','computed','cleanup_scheduled')) OR
    EXISTS(SELECT 1 FROM ai_job_observers o LEFT JOIN ai_job_aggregates a ON a.job_id=o.job_id WHERE a.finish_task_id IS NULL) THEN
  RAISE EXCEPTION 'retained results, continuations, outputs or job observers need reconciliation';
 END IF;
END $$;
CREATE SCHEMA async_v2_archive;
REVOKE ALL ON SCHEMA async_v2_archive FROM PUBLIC;
-- Snapshots preserve the original per-phase budgets and result identities.
CREATE TABLE async_v2_archive.task_details AS SELECT * FROM tasks;
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['task_deliveries','results','executions','task_continuations','queue_budgets','committer_members','write_budget','write_permits','ai_job_observers','coordination_handover','coordination_messages'] LOOP
  EXECUTE format('ALTER TABLE %I SET SCHEMA async_v2_archive',name);
 END LOOP;
END $$;
INSERT INTO control_settings(key,value) VALUES('v3_contract','archived') ON CONFLICT(key) DO UPDATE SET value='archived';

ALTER TABLE tasks DROP COLUMN layout_version,DROP COLUMN shard,DROP COLUMN priority,
 DROP COLUMN commit_attempts,DROP COLUMN max_commit_attempts,DROP COLUMN commit_deadline,
 DROP COLUMN result_id,DROP COLUMN recoveries,DROP COLUMN max_recoveries,
 DROP COLUMN result_sends,DROP COLUMN max_result_sends;
ALTER TABLE tasks DROP CONSTRAINT tasks_phase_check;
ALTER TABLE tasks ALTER COLUMN phase SET DEFAULT 'execute';
ALTER TABLE tasks ADD CONSTRAINT tasks_phase_check CHECK(phase IN ('execute','reconcile'));
