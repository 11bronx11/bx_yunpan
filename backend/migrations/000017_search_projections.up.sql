BEGIN;

-- Search is a rebuildable projection.  It deliberately has no foreign keys
-- back into the business tables so a stale index can never block a business
-- write or become an alternate permission authority.
CREATE TABLE search_index_versions (
    id uuid PRIMARY KEY,
    backend varchar(32) NOT NULL,
    projection_type varchar(32) NOT NULL CHECK (projection_type IN ('content', 'file')),
    target_version varchar(128) NOT NULL,
    state varchar(32) NOT NULL CHECK (state IN ('building', 'active', 'failed', 'retired')),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz
);

CREATE UNIQUE INDEX uq_search_index_versions_active
    ON search_index_versions(backend, projection_type)
    WHERE state = 'active';

CREATE TABLE search_content_projections (
    id uuid PRIMARY KEY,
    backend varchar(32) NOT NULL,
    target_version varchar(128) NOT NULL,
    object_id uuid NOT NULL,
    document_version_id uuid NOT NULL,
    chunk_id uuid NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    pipeline_version varchar(64) NOT NULL,
    model_version varchar(128) NOT NULL,
    index_epoch bigint NOT NULL CHECK (index_epoch > 0),
    content text NOT NULL,
    vector_ref text,
    state varchar(32) NOT NULL CHECK (state IN ('pending', 'indexed', 'tombstoned', 'failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    last_error text,
    indexed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (backend, target_version, chunk_id)
);

CREATE INDEX idx_search_content_object ON search_content_projections(object_id, document_version_id, state);
CREATE INDEX idx_search_content_version ON search_content_projections(backend, target_version, updated_at);

CREATE TABLE search_file_projections (
    id uuid PRIMARY KEY,
    backend varchar(32) NOT NULL,
    target_version varchar(128) NOT NULL,
    file_entry_id uuid NOT NULL,
    owner_id uuid NOT NULL,
    folder_id uuid NOT NULL,
    object_id uuid NOT NULL,
    file_version bigint NOT NULL CHECK (file_version > 0),
    acl_version bigint NOT NULL DEFAULT 1 CHECK (acl_version > 0),
    name text NOT NULL,
    mime_type varchar(255) NOT NULL,
    tombstone boolean NOT NULL DEFAULT false,
    index_epoch bigint NOT NULL CHECK (index_epoch > 0),
    state varchar(32) NOT NULL CHECK (state IN ('pending', 'indexed', 'tombstoned', 'failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    last_error text,
    indexed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (backend, target_version, file_entry_id)
);

CREATE INDEX idx_search_file_owner ON search_file_projections(owner_id, folder_id, tombstone, state);
CREATE INDEX idx_search_file_object ON search_file_projections(object_id, state);

COMMIT;
