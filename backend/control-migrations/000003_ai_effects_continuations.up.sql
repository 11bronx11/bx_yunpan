ALTER TABLE tasks ADD COLUMN effect_target text NOT NULL DEFAULT 'core' CHECK (effect_target IN ('none','core','ai'));
UPDATE tasks SET effect_target='none' WHERE kind IN ('ai.index','search.project');
ALTER TABLE results ADD COLUMN effect_target text NOT NULL DEFAULT 'core' CHECK (effect_target IN ('none','core','ai'));
UPDATE results r SET effect_target=t.effect_target FROM tasks t WHERE t.id=r.task_id;
CREATE INDEX tasks_target_results_due ON tasks(effect_target,not_before,id) WHERE state IN ('computed','convergence_pending');
ALTER TABLE committer_members ADD COLUMN effect_target text NOT NULL DEFAULT 'core' CHECK (effect_target IN ('core','ai'));
ALTER TABLE committer_members DROP CONSTRAINT committer_members_pkey;
ALTER TABLE committer_members ADD PRIMARY KEY(id,effect_target);
CREATE TABLE task_continuations (
 parent_task_id uuid NOT NULL REFERENCES tasks(id), accepted_result_id uuid NOT NULL REFERENCES results(id),
 ordinal integer NOT NULL CHECK(ordinal>=0), spec jsonb NOT NULL, spec_digest text NOT NULL,
 state text NOT NULL CHECK(state IN ('blocked','released','suppressed')),
 child_task_id uuid REFERENCES tasks(id), created_at timestamptz NOT NULL DEFAULT clock_timestamp(), released_at timestamptz,
 PRIMARY KEY(accepted_result_id,ordinal), CHECK ((state='released')=(child_task_id IS NOT NULL))
);
CREATE INDEX task_continuations_pending ON task_continuations(parent_task_id) WHERE state='blocked';
CREATE TABLE ai_job_task_links (
 job_id uuid NOT NULL, task_id uuid NOT NULL REFERENCES tasks(id), target text NOT NULL,
 required boolean NOT NULL, requested_identity text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(job_id,task_id,target)
);
CREATE TABLE ai_job_aggregates (
 job_id uuid PRIMARY KEY, revision bigint NOT NULL DEFAULT 0,
 requested_targets jsonb NOT NULL, outcomes jsonb NOT NULL DEFAULT '{}',
 finish_task_id uuid REFERENCES tasks(id), updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO queue_budgets(queue,inflight_limit,owner_limit,result_high)
SELECT q,4,2,128 FROM unnest(ARRAY['ai-content','ai-embedding','ai-summary','ai-project-text','ai-project-vector','ai-maintenance']) q ON CONFLICT DO NOTHING;
