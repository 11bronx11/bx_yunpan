CREATE TABLE ai_resource_intents (
 object_id uuid NOT NULL REFERENCES file_objects(id), content_generation bigint NOT NULL CHECK(content_generation>0),
 source_seq bigint NOT NULL DEFAULT 1 CHECK(source_seq>=0), text_seq bigint NOT NULL DEFAULT 1 CHECK(text_seq>=0),
 vector_seq bigint NOT NULL DEFAULT 1 CHECK(vector_seq>=0), summary_seq bigint NOT NULL DEFAULT 1 CHECK(summary_seq>=0),
 profile_id text NOT NULL DEFAULT 'default', profile_revision text NOT NULL DEFAULT 'v1',
 last_request_id uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(object_id,content_generation)
);
CREATE TABLE ai_job_requests (
 job_id uuid PRIMARY KEY REFERENCES async_tasks(id), owner_id uuid NOT NULL, file_id uuid NOT NULL,
 object_id uuid NOT NULL, content_generation bigint NOT NULL,
 requested_targets jsonb NOT NULL, profile_revisions jsonb NOT NULL, requested_seqs jsonb NOT NULL,
 request_digest text NOT NULL, capability_outcomes jsonb NOT NULL DEFAULT '{}', result_version_refs jsonb NOT NULL DEFAULT '{}',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), finished_at timestamptz
);
CREATE INDEX ai_job_requests_object ON ai_job_requests(object_id,content_generation);
