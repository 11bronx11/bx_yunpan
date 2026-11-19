-- Expand only. v2 results, budgets and handover evidence remain intact.
-- Direct inserts omit retired columns before and after contract. Expansion
-- must accept them while this legacy NOT NULL column still exists.
ALTER TABLE tasks ALTER COLUMN shard SET DEFAULT 0;
ALTER TABLE tasks DROP CONSTRAINT tasks_phase_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_phase_check CHECK (phase IN ('compute','commit','command','reconcile','execute'));
ALTER TABLE tasks ADD COLUMN dispatch_state text NOT NULL DEFAULT 'pending'
 CHECK (dispatch_state IN ('pending','publishing','confirmed','consumed','stopped'));
ALTER TABLE tasks ADD COLUMN dispatch_not_before timestamptz NOT NULL DEFAULT clock_timestamp();
ALTER TABLE tasks ADD COLUMN publish_token uuid;
ALTER TABLE tasks ADD COLUMN publish_lease_until timestamptz;
ALTER TABLE tasks ADD COLUMN publish_confirmed_at timestamptz;
ALTER TABLE tasks ADD COLUMN receipt_check_claimed boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN receipt_check_deadline timestamptz;
ALTER TABLE ai_job_task_links ADD COLUMN completion_refs jsonb NOT NULL DEFAULT '{}'::jsonb;
CREATE INDEX tasks_v3_publish_due ON tasks(dispatch_not_before,id)
 WHERE protocol_version=3 AND state='pending' AND dispatch_state IN ('pending','publishing');
CREATE INDEX tasks_v3_running ON tasks(lease_until,id) WHERE protocol_version=3 AND state='running';
CREATE OR REPLACE FUNCTION fence_coordination_writer() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE writer text := current_setting('bx.coordination_protocol',true);
        active text := (SELECT value FROM control_settings WHERE key='coordination_protocol');
BEGIN
 IF (NEW.protocol_version=3 OR (TG_OP='UPDATE' AND OLD.protocol_version=3)) AND writer IS DISTINCT FROM 'v3' THEN
  RAISE EXCEPTION 'v3 coordination requires a v3 writer';
 END IF;
 IF NEW.protocol_version=2 AND writer NOT IN ('v2','v3') OR
    NEW.protocol_version=2 AND writer IS NULL THEN
  RAISE EXCEPTION 'v2 coordination requires a v2 writer or compatible v3 writer';
 END IF;
 IF active IN ('handover-v3','v3') AND writer IS DISTINCT FROM 'v3' THEN
  RAISE EXCEPTION 'old coordination writer is fenced';
 END IF;
 IF NEW.protocol_version=1 AND active IN ('handover','v2','handover-v3','v3') THEN
  RAISE EXCEPTION 'v1 coordination is retired';
 END IF;
 IF NEW.protocol_version=2 AND active='v3' THEN
  RAISE EXCEPTION 'v2 task writes are retired';
 END IF;
 RETURN NEW;
END $$;
