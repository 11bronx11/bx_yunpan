-- Run in a maintenance window after fencing old producers/consumers. This
-- migration takes table locks and intentionally makes no online-DDL claim.
ALTER TABLE outbox_events ADD COLUMN seq bigserial;
ALTER TABLE outbox_events ADD COLUMN event_xid xid8 NOT NULL DEFAULT pg_current_xact_id();
-- Historical rows share this migration's committed xid and retain a stable seq.
CREATE UNIQUE INDEX outbox_events_seq ON outbox_events(seq);
CREATE INDEX outbox_events_safe_position ON outbox_events(event_xid,seq);
-- New input is append-only; transport fanout no longer occurs in business txs.
DO $$ DECLARE r record; BEGIN
 FOR r IN SELECT tgname FROM pg_trigger WHERE tgrelid='outbox_deliveries'::regclass AND NOT tgisinternal
 LOOP EXECUTE format('DROP TRIGGER %I ON outbox_deliveries', r.tgname); END LOOP;
END $$;
CREATE TABLE async_commit_receipts (
    semantic_key text PRIMARY KEY, input_digest text NOT NULL, result_digest text NOT NULL,
    result_id uuid NOT NULL, outcome text NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE file_objects ADD COLUMN content_generation bigint NOT NULL DEFAULT 1 CHECK(content_generation > 0);
ALTER TABLE file_objects ADD COLUMN gc_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE file_objects ADD COLUMN gc_eligible_at timestamptz;
UPDATE file_objects SET gc_eligible_at=updated_at WHERE reference_count=0;
CREATE TABLE user_quota_usage (
    owner_id uuid PRIMARY KEY REFERENCES users(id), occupied_bytes bigint NOT NULL CHECK(occupied_bytes >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO user_quota_usage(owner_id,occupied_bytes) SELECT id,used_logical_bytes+reserved_bytes FROM users;
CREATE TABLE owner_async_epochs (
    owner_id uuid PRIMARY KEY REFERENCES users(id), content_epoch bigint NOT NULL DEFAULT 0 CHECK(content_epoch >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO owner_async_epochs(owner_id) SELECT id FROM users;
CREATE INDEX upload_sessions_expiry_keyset ON upload_sessions(expires_at,id)
    WHERE status IN ('initializing','created','uploading');
