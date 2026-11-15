-- Reconcile quota presentation before reverting code. No event/receipt deletion
-- is performed by normal startup; this explicit rollback drops new metadata.
DROP INDEX upload_sessions_expiry_keyset;
DROP TABLE owner_async_epochs,user_quota_usage,async_commit_receipts;
ALTER TABLE file_objects DROP COLUMN content_generation, DROP COLUMN gc_generation, DROP COLUMN gc_eligible_at;
DROP INDEX outbox_events_safe_position;
DROP INDEX outbox_events_seq;
ALTER TABLE outbox_events DROP COLUMN event_xid, DROP COLUMN seq;
