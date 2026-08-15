BEGIN;

CREATE TABLE folders (
    id uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id uuid REFERENCES folders(id),
    name varchar(255) NOT NULL,
    name_normalized varchar(255) NOT NULL,
    deleted_at timestamptz,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_folders_root
    ON folders(owner_id)
    WHERE parent_id IS NULL AND deleted_at IS NULL;

CREATE UNIQUE INDEX uq_folders_active_name
    ON folders(owner_id, parent_id, name_normalized)
    WHERE parent_id IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX idx_folders_parent_active
    ON folders(owner_id, parent_id)
    WHERE deleted_at IS NULL;

INSERT INTO folders (id, owner_id, parent_id, name, name_normalized)
SELECT gen_random_uuid(), id, NULL, '/', '/'
FROM users;

COMMIT;
