BEGIN;

ALTER TABLE file_objects
    ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1;
ALTER TABLE file_objects
    ADD CONSTRAINT file_objects_version_check CHECK (version > 0);

ALTER TABLE async_tasks
    ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1;
ALTER TABLE async_tasks
    ADD CONSTRAINT async_tasks_version_check CHECK (version > 0);

COMMIT;
