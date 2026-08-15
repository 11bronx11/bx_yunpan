BEGIN;

CREATE TABLE shares (
    id uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_entry_id uuid NOT NULL REFERENCES file_entries(id),
    key_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz,
    revoked_at timestamptz,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_shares_owner_created ON shares(owner_id, created_at DESC);

CREATE TABLE share_imports (
    id uuid PRIMARY KEY,
    share_id uuid NOT NULL REFERENCES shares(id),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_folder_id uuid NOT NULL REFERENCES folders(id),
    imported_entry_id uuid NOT NULL REFERENCES file_entries(id),
    idempotency_key varchar(128) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, share_id, idempotency_key)
);

CREATE TABLE object_variants (
    id uuid PRIMARY KEY,
    object_id uuid NOT NULL REFERENCES file_objects(id) ON DELETE CASCADE,
    variant_type varchar(64) NOT NULL,
    object_key varchar(1024) NOT NULL UNIQUE,
    mime_type varchar(255) NOT NULL,
    width integer,
    height integer,
    pipeline_version varchar(64) NOT NULL,
    status varchar(32) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (object_id, variant_type, pipeline_version)
);

COMMIT;
