\set ON_ERROR_STOP on
\pset pager off
BEGIN;
SET LOCAL statement_timeout = '30s';

-- Synthetic rows are transaction-local and always rolled back. Keep planner
-- defaults: disabling sequential scans would not prove an index is useful.
INSERT INTO users (id, username, username_normalized, password_hash)
SELECT md5('hc-owner-' || i)::uuid, 'hc-owner-' || i, 'hc-owner-' || i, 'fixture'
FROM generate_series(1, 50) AS i;
INSERT INTO folders (id, owner_id, name, name_normalized)
SELECT md5('hc-folder-' || i)::uuid, md5('hc-owner-' || i)::uuid, '/', '/'
FROM generate_series(1, 50) AS i;

INSERT INTO upload_sessions (id, user_id, folder_id, filename, filename_normalized,
    declared_sha256, mime_type, size_bytes, reserved_bytes, bucket, object_key,
    storage_upload_id, part_size, part_count, status, idempotency_key, expires_at)
SELECT md5('hc-upload-' || i)::uuid,
    md5('hc-owner-' || (1 + i % 50))::uuid, md5('hc-folder-' || (1 + i % 50))::uuid,
    'file-' || i, 'file-' || i, repeat('a', 64), 'text/plain', 128, 0, 'hc', 'hc-' || i,
    '', 128, 1, CASE WHEN i <= 250 THEN 'initializing' ELSE 'completed' END,
    'hc-' || i, now() + interval '1 hour'
FROM generate_series(1, 50000) AS i;

INSERT INTO storage_operations (id, operation_type, idempotency_key, state, available_at, lease_until)
SELECT md5('hc-op-' || i)::uuid, 'multipart.create', 'hc-' || i,
    CASE WHEN i <= 100 THEN 'pending' WHEN i <= 200 THEN 'running' ELSE 'succeeded' END,
    now() - interval '1 minute', CASE WHEN i > 100 AND i <= 200 THEN now() - interval '1 second' END
FROM generate_series(1, 50000) AS i;

INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, event_version, payload, status)
SELECT md5('hc-event-' || i)::uuid, 'upload_session', md5('hc-upload-' || i)::uuid,
    'object.verify_requested.v1', 1, '{}', 'published'
FROM generate_series(1, 50000) AS i;
INSERT INTO outbox_deliveries (event_id, transport, destination, status, available_at, lease_until, lease_token)
SELECT md5('hc-event-' || i)::uuid, 'rabbitmq', 'object.verify.v1@object',
    CASE WHEN i <= 100 THEN 'pending' WHEN i <= 200 THEN 'sending' ELSE 'published' END,
    now() - interval '1 minute', CASE WHEN i > 100 AND i <= 200 THEN now() - interval '1 second' END,
    CASE WHEN i > 100 AND i <= 200 THEN gen_random_uuid() END
FROM generate_series(1, 50000) AS i;

INSERT INTO file_objects (id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count, updated_at)
SELECT md5('hc-object-' || i)::uuid, md5('hc-' || i) || md5('hc-' || i), 128, 'text/plain', 'hc', 'hc-object-' || i,
    'ready', CASE WHEN i <= 100 THEN 0 ELSE 1 END, now() - interval '2 days'
FROM generate_series(1, 50000) AS i;
INSERT INTO file_entries (id, owner_id, folder_id, object_id, name, name_normalized)
SELECT md5('hc-entry-' || i)::uuid, md5('hc-owner-' || (1 + i % 50))::uuid,
    md5('hc-folder-' || (1 + i % 50))::uuid, md5('hc-object-' || i)::uuid,
    CASE WHEN i % 1000 = 0 THEN 'needle-report-' || i ELSE 'document-' || i END,
    CASE WHEN i % 1000 = 0 THEN 'needle-report-' || i ELSE 'document-' || i END
FROM generate_series(101, 50000) AS i;
INSERT INTO async_tasks (id, task_type, dedupe_key, resource_id, status, updated_at)
SELECT md5('hc-task-' || i)::uuid, 'ai.reprocess', 'hc-' || i, md5('hc-object-' || i)::uuid,
    CASE WHEN i <= 100 THEN 'running' ELSE 'completed' END, now() - interval '1 hour'
FROM generate_series(1, 50000) AS i;

ANALYZE upload_sessions;
ANALYZE storage_operations;
ANALYZE outbox_deliveries;
ANALYZE file_entries;
ANALYZE file_objects;
ANALYZE async_tasks;

SELECT 'active upload list (application predicate)' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM upload_sessions WHERE user_id = md5('hc-owner-1')::uuid
AND status IN ('initializing','created','uploading','completing','verifying') AND expires_at > now()
ORDER BY created_at DESC;

SELECT 'active upload filename conflict (application predicate)' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM upload_sessions WHERE user_id = md5('hc-owner-1')::uuid AND folder_id = md5('hc-folder-1')::uuid
AND filename_normalized = 'file-50' AND expires_at > now()
AND status IN ('initializing','created','uploading','completing','verifying');

SELECT 'storage claim (application predicate)' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM storage_operations WHERE operation_type IN ('multipart.create','multipart.complete','multipart.abort','canonical.ensure','staging.delete')
AND state IN ('pending','failed','running') AND available_at <= now() AND (lease_until IS NULL OR lease_until <= now())
ORDER BY available_at, created_at LIMIT 100 FOR UPDATE SKIP LOCKED;

SELECT 'storage ready only (diagnostic predicate)' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM storage_operations WHERE operation_type = 'multipart.create' AND state IN ('pending','failed') AND available_at <= now()
ORDER BY available_at, created_at LIMIT 100;

SELECT 'storage expired leases (diagnostic predicate)' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM storage_operations WHERE state = 'running' AND lease_until <= now() ORDER BY lease_until LIMIT 100;

SELECT 'outbox claim (application predicate)' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM outbox_deliveries WHERE transport = 'rabbitmq'
AND ((status IN ('pending','failed') AND available_at <= now()) OR (status = 'sending' AND lease_until <= now()))
ORDER BY available_at, event_id LIMIT 20 FOR UPDATE SKIP LOCKED;

SELECT 'filename search with owner filter' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM file_entries WHERE owner_id = md5('hc-owner-1')::uuid AND deleted_at IS NULL
AND name_normalized LIKE '%needle%' LIMIT 100;

SELECT 'filename search after normal GIN pending-list maintenance' AS probe;
SELECT gin_clean_pending_list('idx_file_entries_name_trgm');
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM file_entries WHERE owner_id = md5('hc-owner-1')::uuid AND deleted_at IS NULL
AND name_normalized LIKE '%needle%' LIMIT 100;

SELECT 'unreferenced objects eligible for recovery' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM file_objects WHERE status IN ('ready','deleting') AND reference_count = 0
AND updated_at < now() - interval '1 day' ORDER BY updated_at LIMIT 100;

SELECT 'async task recovery (application predicate)' AS probe;
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM async_tasks WHERE task_type = 'ai.reprocess' AND resource_id IS NOT NULL
AND status IN ('pending','running','failed') AND updated_at <= now() - interval '5 minutes'
ORDER BY updated_at LIMIT 100;

ROLLBACK;
