BEGIN;

CREATE TABLE file_objects (
    id uuid PRIMARY KEY,
    sha256 char(64) NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    mime_type varchar(255) NOT NULL,
    bucket varchar(128) NOT NULL,
    object_key varchar(1024) NOT NULL UNIQUE,
    status varchar(32) NOT NULL CHECK (status IN ('ready', 'deleting', 'deleted')),
    reference_count bigint NOT NULL DEFAULT 0 CHECK (reference_count >= 0),
    verified_at timestamptz,
    deleted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (sha256, size_bytes)
);

CREATE TABLE file_entries (
    id uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    folder_id uuid NOT NULL REFERENCES folders(id),
    object_id uuid NOT NULL REFERENCES file_objects(id),
    name varchar(255) NOT NULL,
    name_normalized varchar(255) NOT NULL,
    deleted_at timestamptz,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_file_entries_active_name
    ON file_entries(owner_id, folder_id, name_normalized)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_file_entries_object_active ON file_entries(object_id) WHERE deleted_at IS NULL;

CREATE TABLE upload_sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    folder_id uuid NOT NULL REFERENCES folders(id),
    filename varchar(255) NOT NULL,
    filename_normalized varchar(255) NOT NULL,
    declared_sha256 char(64) NOT NULL,
    mime_type varchar(255) NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    reserved_bytes bigint NOT NULL CHECK (reserved_bytes >= 0),
    bucket varchar(128) NOT NULL,
    object_key varchar(1024) NOT NULL,
    storage_upload_id varchar(512) NOT NULL,
    part_size bigint NOT NULL,
    part_count integer NOT NULL,
    status varchar(32) NOT NULL CHECK (status IN ('created', 'uploading', 'verifying', 'completed', 'failed', 'aborted', 'expired')),
    idempotency_key varchar(128) NOT NULL,
    completed_object_id uuid REFERENCES file_objects(id),
    completed_entry_id uuid REFERENCES file_entries(id),
    error_code varchar(128),
    expires_at timestamptz NOT NULL,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);

CREATE TABLE upload_parts (
    session_id uuid NOT NULL REFERENCES upload_sessions(id) ON DELETE CASCADE,
    part_number integer NOT NULL CHECK (part_number > 0),
    etag varchar(512) NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    checksum varchar(512),
    completed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, part_number)
);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY,
    aggregate_type varchar(64) NOT NULL,
    aggregate_id uuid NOT NULL,
    event_type varchar(128) NOT NULL,
    event_version integer NOT NULL,
    payload jsonb NOT NULL,
    status varchar(32) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'published', 'failed')),
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempt integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_outbox_dispatch ON outbox_events(status, available_at, created_at);

CREATE TABLE event_consumptions (
    event_id uuid NOT NULL,
    consumer_name varchar(128) NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer_name)
);

CREATE TABLE async_tasks (
    id uuid PRIMARY KEY,
    owner_id uuid REFERENCES users(id) ON DELETE CASCADE,
    task_type varchar(128) NOT NULL,
    dedupe_key varchar(255) NOT NULL,
    resource_type varchar(64),
    resource_id uuid,
    status varchar(32) NOT NULL,
    progress integer NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    attempt integer NOT NULL DEFAULT 0,
    error_code varchar(128),
    error_message text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_type, dedupe_key)
);

COMMIT;
