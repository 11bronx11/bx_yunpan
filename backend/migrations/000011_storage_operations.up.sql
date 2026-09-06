BEGIN;

CREATE TABLE storage_operations (
    id uuid PRIMARY KEY,
    operation_type varchar(64) NOT NULL CHECK (operation_type IN (
        'multipart.create', 'multipart.complete', 'multipart.abort',
        'canonical.ensure', 'staging.delete', 'object.delete'
    )),
    idempotency_key varchar(512) NOT NULL UNIQUE,
    upload_session_id uuid REFERENCES upload_sessions(id) ON DELETE CASCADE,
    object_id uuid REFERENCES file_objects(id) ON DELETE CASCADE,
    source_bucket varchar(255),
    source_key varchar(1024),
    target_bucket varchar(255),
    target_key varchar(1024),
    storage_upload_id varchar(255),
    state varchar(32) NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'succeeded', 'failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE INDEX idx_storage_operations_claim
    ON storage_operations(state, available_at, lease_until, created_at);
CREATE INDEX idx_storage_operations_upload
    ON storage_operations(upload_session_id, operation_type, state);
CREATE INDEX idx_storage_operations_object
    ON storage_operations(object_id, operation_type, state);

COMMENT ON TABLE storage_operations IS
    'Durable, idempotent external object-storage operations; contains no credentials, URLs, or content.';

COMMIT;
