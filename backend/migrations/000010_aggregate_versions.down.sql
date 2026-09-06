BEGIN;

ALTER TABLE async_tasks
    DROP CONSTRAINT IF EXISTS async_tasks_version_check,
    DROP COLUMN IF EXISTS version;
ALTER TABLE file_objects
    DROP CONSTRAINT IF EXISTS file_objects_version_check,
    DROP COLUMN IF EXISTS version;

COMMIT;
