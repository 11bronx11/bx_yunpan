BEGIN;

CREATE TABLE owner_content_epochs (
    owner_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    content_epoch bigint NOT NULL DEFAULT 1 CHECK (content_epoch > 0),
    acl_epoch bigint NOT NULL DEFAULT 1 CHECK (acl_epoch > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO owner_content_epochs (owner_id, content_epoch, acl_epoch, updated_at)
SELECT id, 1, 1, now() FROM users
ON CONFLICT (owner_id) DO NOTHING;

COMMIT;
