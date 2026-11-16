-- Stop AI writers first. Downgrading with accepted AI results is unsafe.
DO $$ BEGIN IF EXISTS(SELECT 1 FROM tasks WHERE effect_target='ai' AND state NOT IN ('succeeded','superseded','rejected','failed')) THEN RAISE EXCEPTION 'unsettled AI effects prevent downgrade'; END IF; END $$;
DROP TABLE ai_job_aggregates,ai_job_task_links,task_continuations;
DELETE FROM committer_members WHERE effect_target='ai';
ALTER TABLE committer_members DROP CONSTRAINT committer_members_pkey;
ALTER TABLE committer_members ADD PRIMARY KEY(id);
ALTER TABLE committer_members DROP COLUMN effect_target;
DROP INDEX tasks_target_results_due;
ALTER TABLE results DROP COLUMN effect_target;
ALTER TABLE tasks DROP COLUMN effect_target;
DELETE FROM queue_budgets WHERE queue LIKE 'ai-%';
