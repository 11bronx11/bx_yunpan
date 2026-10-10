BEGIN;
CREATE TABLE task_executions (
 task_id text PRIMARY KEY,
 payload_sha256 text NOT NULL,
 task_type text NOT NULL,
 queue text NOT NULL,
 status text NOT NULL CHECK (status IN ('running','retry','finished')),
 lease_token uuid,
 lease_until timestamptz,
 available_at timestamptz NOT NULL,
 retry_count integer NOT NULL CHECK (retry_count >= 0),
 max_retry integer NOT NULL CHECK (max_retry >= 0),
 timeout_ms bigint NOT NULL CHECK (timeout_ms > 0),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_task_execution_leases ON task_executions(lease_until) WHERE status='running';
CREATE INDEX idx_outbox_unconfirmed_execution ON outbox_deliveries(published_at,event_id,destination)
 WHERE transport='rabbitmq' AND status='published';
COMMIT;
