BEGIN;

-- Expand only: the legacy status/attempt columns remain authoritative for old
-- binaries until the delivery-aware relay has been deployed everywhere.
ALTER TABLE outbox_events
    ADD COLUMN IF NOT EXISTS message_kind varchar(32),
    ADD COLUMN IF NOT EXISTS aggregate_version bigint,
    ADD COLUMN IF NOT EXISTS producer varchar(128),
    ADD COLUMN IF NOT EXISTS occurred_at timestamptz;

ALTER TABLE outbox_events
    ADD CONSTRAINT outbox_events_message_kind_check
        CHECK (message_kind IS NULL OR message_kind IN ('domain_event', 'command')),
    ADD CONSTRAINT outbox_events_aggregate_version_check
        CHECK (aggregate_version IS NULL OR aggregate_version > 0);

CREATE TABLE outbox_deliveries (
    event_id uuid NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    transport varchar(32) NOT NULL,
    destination varchar(255) NOT NULL,
    status varchar(32) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'published', 'failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    last_error text,
    PRIMARY KEY (event_id, transport, destination)
);

CREATE INDEX idx_outbox_deliveries_poll
    ON outbox_deliveries(transport, status, available_at, event_id);

-- Backfill metadata without fabricating aggregate versions. Unknown legacy
-- event types intentionally keep NULL metadata and are handled as unrouted.
UPDATE outbox_events
SET message_kind = CASE
        WHEN event_type IN (
            'object.verify_requested.v1',
            'object.gc_requested.v1',
            'ai.reprocess_requested.v1',
            'ai.index_requested.v1'
        ) THEN 'command'
        WHEN event_type IN (
            'file.created.v1',
            'share.imported.v1',
            'object.ready.v1'
        ) THEN 'domain_event'
        ELSE message_kind
    END,
    producer = COALESCE(producer, 'legacy-core'),
    occurred_at = COALESCE(occurred_at, created_at)
WHERE producer IS NULL OR occurred_at IS NULL OR message_kind IS NULL;

-- Legacy rows are copied to the explicit destinations used by the compatible
-- relay. Published rows are marked published so a migration can never enqueue
-- an event that an older relay already acknowledged.
WITH routes(event_type, transport, destination) AS (
    VALUES
        ('file.created.v1', 'asynq', 'file.created.v1@maintenance'),
        ('share.imported.v1', 'asynq', 'share.imported.v1@maintenance'),
        ('object.verify_requested.v1', 'asynq', 'object.verify_requested.v1@object'),
        ('object.ready.v1', 'asynq', 'object.ready.v1@media'),
        ('object.ready.v1', 'asynq', 'ai.index_requested.v1@ai'),
        ('object.gc_requested.v1', 'asynq', 'object.gc_requested.v1@maintenance'),
        ('ai.reprocess_requested.v1', 'asynq', 'ai.reprocess_requested.v1@ai'),
        ('ai.index_requested.v1', 'asynq', 'ai.index_requested.v1@ai')
)
INSERT INTO outbox_deliveries (
    event_id, transport, destination, status, attempt, available_at, published_at
)
SELECT
    e.id,
    r.transport,
    r.destination,
    CASE WHEN e.status = 'published' THEN 'published' ELSE e.status END,
    GREATEST(e.attempt, 0),
    COALESCE(e.available_at, e.created_at),
    CASE WHEN e.status = 'published' THEN e.published_at ELSE NULL END
FROM outbox_events AS e
JOIN routes AS r ON r.event_type = e.event_type
WHERE e.status IN ('pending', 'failed', 'published')
ON CONFLICT (event_id, transport, destination) DO NOTHING;

COMMENT ON COLUMN outbox_events.event_version IS
    'Payload schema version; transport delivery state is stored in outbox_deliveries.';
COMMENT ON COLUMN outbox_events.aggregate_version IS
    'Committed aggregate version at event creation; NULL means unknown for legacy rows.';

COMMIT;
