-- Expansion only: retain v1 history, queues and budget columns for handover.
ALTER TABLE tasks DROP CONSTRAINT tasks_state_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_state_check CHECK(state IN
 ('ready','dispatched','running','retry_wait','computed','succeeded','superseded','rejected','exhausted','convergence_pending','failed','quarantined','needs_attention','pending','done','stopped'));
ALTER TABLE tasks ADD COLUMN protocol_version integer NOT NULL DEFAULT 1;
ALTER TABLE tasks ADD COLUMN outcome text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN phase text NOT NULL DEFAULT 'compute' CHECK(phase IN ('compute','commit','command','reconcile'));
ALTER TABLE tasks ADD COLUMN commit_attempts integer NOT NULL DEFAULT 0 CHECK(commit_attempts>=0);
ALTER TABLE tasks ADD COLUMN max_commit_attempts integer NOT NULL DEFAULT 12 CHECK(max_commit_attempts>0);
CREATE TABLE task_deliveries (
 task_id uuid PRIMARY KEY REFERENCES tasks(id), generation bigint NOT NULL,
 state text NOT NULL CHECK(state IN ('pending','publishing','confirmed','consumed','stopped')),
 not_before timestamptz NOT NULL, token uuid, lease_until timestamptz,
 sends integer NOT NULL DEFAULT 0, confirmed_at timestamptz,
 last_error text NOT NULL DEFAULT '', updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX task_deliveries_due ON task_deliveries(not_before,task_id) WHERE state IN ('pending','publishing');
CREATE INDEX tasks_v2_leases ON tasks(lease_until,id) WHERE protocol_version=2 AND state='running';
CREATE INDEX tasks_v2_due ON tasks(not_before,id) WHERE protocol_version=2 AND state='pending';
CREATE TABLE coordination_handover (
 task_id uuid PRIMARY KEY REFERENCES tasks(id), snapshot jsonb NOT NULL,
 disposition text NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE coordination_messages (
 identity text PRIMARY KEY, notice jsonb NOT NULL, disposition text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO control_settings(key,value) SELECT 'coordination_protocol',CASE WHEN EXISTS(SELECT 1 FROM tasks) THEN 'v1' ELSE 'v2' END;
-- An old image cannot mutate transferred work, even when somebody bypasses its
-- startup check. v2 pools explicitly identify their protocol on every connection.
CREATE FUNCTION fence_coordination_writer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.protocol_version=2 OR (TG_OP='UPDATE' AND OLD.protocol_version=2))
    AND current_setting('bx.coordination_protocol',true) IS DISTINCT FROM 'v2' THEN
  RAISE EXCEPTION 'v2 coordination requires a v2 writer';
 END IF;
 IF NEW.protocol_version=1 AND (SELECT value FROM control_settings WHERE key='coordination_protocol') IN ('handover','v2') THEN
  RAISE EXCEPTION 'v1 coordination is retired';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER coordination_writer BEFORE INSERT OR UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION fence_coordination_writer();
