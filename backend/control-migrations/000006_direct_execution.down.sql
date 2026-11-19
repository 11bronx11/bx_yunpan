DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM tasks WHERE protocol_version=3) OR
    EXISTS(SELECT 1 FROM control_settings WHERE key='coordination_protocol' AND value IN ('v3','handover-v3')) THEN
  RAISE EXCEPTION 'v3 tasks require forward repair, not down migration';
 END IF;
END $$;
DROP INDEX tasks_v3_publish_due;
DROP INDEX tasks_v3_running;
ALTER TABLE ai_job_task_links DROP COLUMN completion_refs;
ALTER TABLE tasks DROP COLUMN dispatch_state, DROP COLUMN dispatch_not_before,
 DROP COLUMN publish_token, DROP COLUMN publish_lease_until, DROP COLUMN publish_confirmed_at,
 DROP COLUMN receipt_check_claimed, DROP COLUMN receipt_check_deadline;
ALTER TABLE tasks DROP CONSTRAINT tasks_phase_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_phase_check CHECK (phase IN ('compute','commit','command','reconcile'));
-- Keep the compatible fence; no active v3 state is permitted above.
