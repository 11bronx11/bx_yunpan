BEGIN;
DROP TABLE IF EXISTS async_tasks;
DROP TABLE IF EXISTS event_consumptions;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS upload_parts;
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS file_entries;
DROP TABLE IF EXISTS file_objects;
COMMIT;
