-- Attach migrated jobs to an already frozen intent without changing task input
-- identities, execution budgets, grants, or accepted results.
CREATE TABLE ai_job_observers (
 request_id uuid NOT NULL, job_id uuid NOT NULL, owner_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,job_id)
);
