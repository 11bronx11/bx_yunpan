BEGIN;

-- Durable inbox/checkpoint for the non-transactional external publish phase.
-- It is separate from event_consumptions so an external failure remains
-- retryable while the canonical PostgreSQL projection is already committed.
CREATE TABLE search_projection_checkpoints (
    event_id uuid NOT NULL,
    consumer_name varchar(128) NOT NULL,
    state varchar(32) NOT NULL CHECK (state IN ('pending', 'publishing', 'indexed', 'tombstoned', 'failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    last_error text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer_name)
);

CREATE INDEX idx_search_projection_checkpoints_state
    ON search_projection_checkpoints(consumer_name, state, updated_at);

COMMIT;
