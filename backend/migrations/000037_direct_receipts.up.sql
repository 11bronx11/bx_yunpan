ALTER TABLE async_commit_receipts ADD COLUMN protocol_version integer NOT NULL DEFAULT 2;
ALTER TABLE async_commit_receipts ADD COLUMN task_id uuid;
ALTER TABLE async_commit_receipts ADD COLUMN effect_kind text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX async_receipt_v3_task ON async_commit_receipts(task_id) WHERE protocol_version=3;
