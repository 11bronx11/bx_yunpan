BEGIN;

CREATE TABLE workflow_instances (
    workflow_id uuid PRIMARY KEY,
    workflow_type varchar(64) NOT NULL,
    business_key varchar(255) NOT NULL UNIQUE,
    state varchar(32) NOT NULL CHECK (state IN ('pending', 'running', 'succeeded', 'compensating', 'failed', 'manual_intervention')),
    deadline timestamptz,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workflow_steps (
    workflow_id uuid NOT NULL REFERENCES workflow_instances(workflow_id) ON DELETE CASCADE,
    step_name varchar(64) NOT NULL,
    command_id uuid NOT NULL UNIQUE,
    state varchar(32) NOT NULL CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'compensating', 'compensated', 'manual_intervention')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    compensation_state varchar(32) NOT NULL DEFAULT 'not_required',
    last_error text,
    started_at timestamptz,
    completed_at timestamptz,
    PRIMARY KEY (workflow_id, step_name)
);

CREATE TABLE command_deduplications (
    command_id uuid PRIMARY KEY,
    owner_service varchar(64) NOT NULL,
    request_hash varchar(128) NOT NULL,
    result_ref varchar(255),
    status varchar(32) NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_workflow_instances_state ON workflow_instances(state, updated_at);
CREATE INDEX idx_workflow_steps_state ON workflow_steps(state, started_at);

COMMIT;
