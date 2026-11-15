CREATE TABLE module_leases (
    name text PRIMARY KEY, holder text NOT NULL, token uuid NOT NULL,
    lease_until timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE relay_positions (
    source text PRIMARY KEY, last_xid xid8 NOT NULL DEFAULT '0', last_seq bigint NOT NULL DEFAULT 0,
    version bigint NOT NULL DEFAULT 0, updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE event_inbox (
    id uuid PRIMARY KEY, event_type text NOT NULL, aggregate_id uuid NOT NULL,
    input_digest text NOT NULL, payload jsonb NOT NULL, plan jsonb NOT NULL,
    status text NOT NULL CHECK(status IN ('accepted','quarantined')),
    reason text NOT NULL DEFAULT '', source_xid xid8 NOT NULL, source_seq bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE tasks (
    id uuid PRIMARY KEY, semantic_key text NOT NULL UNIQUE, kind text NOT NULL, contract_version int NOT NULL DEFAULT 1,
    input_digest text NOT NULL, input jsonb NOT NULL, queue text NOT NULL,
    owner_key text NOT NULL, resource_key text NOT NULL, shard_key text NOT NULL,
    layout_version int NOT NULL DEFAULT 1, shard int NOT NULL CHECK(shard >= 0), priority int NOT NULL DEFAULT 1,
    state text NOT NULL CHECK(state IN ('ready','dispatched','running','retry_wait','computed','succeeded','superseded','rejected','exhausted','convergence_pending','failed','quarantined','needs_attention')),
    version bigint NOT NULL DEFAULT 1, generation bigint NOT NULL DEFAULT 0, dispatch_generation bigint NOT NULL DEFAULT 0,
    token uuid, lease_until timestamptz, not_before timestamptz NOT NULL,
    deadline timestamptz NOT NULL, commit_deadline timestamptz,
    attempts int NOT NULL DEFAULT 0 CHECK(attempts >= 0), max_attempts int NOT NULL DEFAULT 12 CHECK(max_attempts > 0),
    dispatches int NOT NULL DEFAULT 0, max_dispatches int NOT NULL DEFAULT 20,
    recoveries int NOT NULL DEFAULT 0, max_recoveries int NOT NULL DEFAULT 12,
    result_sends int NOT NULL DEFAULT 0, max_result_sends int NOT NULL DEFAULT 20,
    timeout_ms bigint NOT NULL, result_id uuid, parent_id uuid REFERENCES tasks(id),
    last_error text NOT NULL DEFAULT '', trace jsonb NOT NULL DEFAULT '{}',
    started_at timestamptz, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(), stopped_at timestamptz
);
CREATE INDEX tasks_due ON tasks(queue,not_before,priority,id) WHERE state IN ('ready','retry_wait');
CREATE INDEX tasks_leases ON tasks(lease_until,id) WHERE state IN ('running','dispatched');
CREATE INDEX tasks_results_due ON tasks(not_before,id) WHERE state IN ('computed','convergence_pending');
CREATE INDEX tasks_owner_active ON tasks(queue,owner_key) WHERE state IN ('running','dispatched');
CREATE INDEX tasks_parent ON tasks(parent_id) WHERE parent_id IS NOT NULL;
CREATE TABLE task_sources (
    task_id uuid NOT NULL REFERENCES tasks(id), source_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(task_id,source_key)
);
CREATE TABLE executions (
    task_id uuid NOT NULL REFERENCES tasks(id), generation bigint NOT NULL, token uuid NOT NULL,
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(), finished_at timestamptz,
    outcome text NOT NULL DEFAULT '', error text NOT NULL DEFAULT '', outputs jsonb NOT NULL DEFAULT '[]',
    PRIMARY KEY(task_id,generation)
);
CREATE TABLE results (
    id uuid PRIMARY KEY, task_id uuid NOT NULL REFERENCES tasks(id), generation bigint NOT NULL,
    input_digest text NOT NULL, result_digest text NOT NULL, body jsonb NOT NULL,
    outcome text NOT NULL DEFAULT '', committed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(), UNIQUE(task_id,generation)
);
CREATE TABLE quarantines (
    identity text PRIMARY KEY, task_id uuid, reason text NOT NULL, details jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE queue_budgets (
    queue text PRIMARY KEY, inflight_limit int NOT NULL CHECK(inflight_limit > 0),
    owner_limit int NOT NULL CHECK(owner_limit > 0), result_high int NOT NULL CHECK(result_high > 0),
    fair_cursor text NOT NULL DEFAULT '', priority_turn bigint NOT NULL DEFAULT 0,
    paused_until timestamptz NOT NULL DEFAULT 'epoch'
);
INSERT INTO queue_budgets(queue,inflight_limit,owner_limit,result_high)
SELECT q, 8, 2, 256 FROM unnest(ARRAY['upload','object','media','gc','ai','search']) q;
CREATE TABLE committer_members (
    id text PRIMARY KEY, lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE write_budget (
    singleton bool PRIMARY KEY DEFAULT true CHECK(singleton), concurrent_limit int NOT NULL DEFAULT 8,
    rate_per_second int NOT NULL DEFAULT 128, available_tokens double precision NOT NULL DEFAULT 128,
    refilled_at timestamptz NOT NULL DEFAULT clock_timestamp(), pressure_until timestamptz NOT NULL DEFAULT 'epoch'
);
INSERT INTO write_budget(singleton) VALUES(true);
CREATE TABLE write_permits (
    token uuid PRIMARY KEY, shard int NOT NULL UNIQUE, lease_until timestamptz NOT NULL
);
CREATE TABLE scan_positions (
    name text PRIMARY KEY, cursor_time timestamptz NOT NULL DEFAULT 'epoch', cursor_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE control_settings (key text PRIMARY KEY, value text NOT NULL);
INSERT INTO control_settings VALUES ('layout_version','1'),('shards','32');
