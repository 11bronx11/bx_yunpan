BEGIN;
ALTER TABLE outbox_deliveries ADD COLUMN not_before timestamptz,
 ADD COLUMN task_retry_count integer NOT NULL DEFAULT 0,
 ADD COLUMN task_max_retry integer,
 ADD COLUMN task_timeout_ms bigint;
CREATE TABLE legacy_task_imports (
 task_id text PRIMARY KEY, event_id uuid NOT NULL, destination text NOT NULL,
 payload_sha256 text NOT NULL, retry_count integer NOT NULL, max_retry integer NOT NULL,
 available_at timestamptz NOT NULL, imported_at timestamptz NOT NULL DEFAULT now()
);
-- Stop legacy producers/consumers and inventory Redis before applying this
-- cutover. Already published deliveries retain their historical ownership.
-- A sending row is ambiguous (publish may have succeeded), so require an
-- explicit operator decision instead of silently replaying it.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM outbox_deliveries WHERE transport='asynq' AND status='sending') THEN
  RAISE EXCEPTION 'Asynq sending deliveries require reconciliation before RabbitMQ cutover';
 END IF;
END $$;
UPDATE outbox_deliveries SET transport='rabbitmq', lease_until=NULL, lease_token=NULL
 WHERE transport='asynq' AND status IN ('pending','failed');
-- Retire only Kafka delivery state, keeping its audit history and payloads.
UPDATE outbox_deliveries SET status='published', published_at=COALESCE(published_at,now()),
 last_error='Kafka transport retired by migration 000023', lease_until=NULL, lease_token=NULL
 WHERE transport='kafka' AND status<>'published';
CREATE TABLE task_results (task_id text PRIMARY KEY, status text NOT NULL CHECK(status IN ('succeeded','dead')), finished_at timestamptz NOT NULL);
CREATE TABLE task_failures (
 task_id text PRIMARY KEY,
 task_type text NOT NULL,
 queue text NOT NULL,
 payload bytea NOT NULL,
 retry_count integer NOT NULL,
 last_error text NOT NULL,
 failed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_rabbitmq_due_deliveries ON outbox_deliveries(not_before,available_at,event_id)
 WHERE transport='rabbitmq' AND status IN ('pending','failed','sending');
COMMIT;
