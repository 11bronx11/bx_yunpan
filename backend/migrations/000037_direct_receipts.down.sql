DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM async_commit_receipts WHERE protocol_version=3) THEN
  RAISE EXCEPTION 'direct receipts require forward repair';
 END IF;
END $$;
DROP INDEX async_receipt_v3_task;
ALTER TABLE async_commit_receipts DROP COLUMN protocol_version, DROP COLUMN task_id, DROP COLUMN effect_kind;
