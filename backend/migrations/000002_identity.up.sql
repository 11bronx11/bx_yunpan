BEGIN;

CREATE TABLE users (
    id uuid PRIMARY KEY,
    username varchar(64) NOT NULL,
    username_normalized varchar(64) NOT NULL UNIQUE,
    email_normalized varchar(254) UNIQUE,
    password_hash text NOT NULL,
    status smallint NOT NULL DEFAULT 1 CHECK (status IN (1, 2)),
    quota_bytes bigint NOT NULL DEFAULT 10737418240 CHECK (quota_bytes >= 0),
    used_logical_bytes bigint NOT NULL DEFAULT 0 CHECK (used_logical_bytes >= 0),
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (used_logical_bytes + reserved_bytes <= quota_bytes)
);

CREATE TABLE refresh_sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    family_id uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_refresh_sessions_user_family ON refresh_sessions(user_id, family_id);
CREATE INDEX idx_refresh_sessions_expires_at ON refresh_sessions(expires_at) WHERE revoked_at IS NULL;

COMMIT;
