BEGIN;

CREATE TABLE outbox_recoveries (
    recovery_key varchar(512) PRIMARY KEY,
    event_id uuid NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    reason varchar(128) NOT NULL,
    resource_type varchar(64) NOT NULL,
    resource_id uuid NOT NULL,
    target_version bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_outbox_recoveries_resource
    ON outbox_recoveries(resource_type, resource_id, updated_at);

COMMENT ON TABLE outbox_recoveries IS
    'Auditable, deterministic recovery nudges for PostgreSQL states whose broker task may be missing.';

COMMIT;
