CREATE EXTENSION IF NOT EXISTS vector;
CREATE TABLE ai_objects (
 object_id uuid NOT NULL, content_generation bigint NOT NULL CHECK(content_generation>0),
 content_hash text NOT NULL, size bigint NOT NULL CHECK(size>=0), media_type text NOT NULL,
 lifecycle_revision bigint NOT NULL DEFAULT 0, tombstoned_at timestamptz,
 desired_source_seq bigint NOT NULL DEFAULT 0, desired_text_seq bigint NOT NULL DEFAULT 0,
 desired_vector_seq bigint NOT NULL DEFAULT 0, desired_summary_seq bigint NOT NULL DEFAULT 0,
 profile_id text NOT NULL, profile_revision text NOT NULL,
 active_source_id uuid, active_content_version_id uuid, active_embedding_set_id uuid, active_summary_id uuid,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(object_id,content_generation)
);
CREATE TABLE ai_sources (
 id uuid PRIMARY KEY, object_id uuid NOT NULL, content_generation bigint NOT NULL, source_seq bigint NOT NULL,
 input_digest text NOT NULL, profile_revision text NOT NULL, parser_id text NOT NULL, parser_version text NOT NULL,
 parser_profile_version text NOT NULL, vision_model_version text NOT NULL DEFAULT '', language text NOT NULL DEFAULT '',
 quality jsonb NOT NULL DEFAULT '{}', warnings jsonb NOT NULL DEFAULT '[]', block_count int NOT NULL CHECK(block_count>=0), source_hash text NOT NULL,
 publication_state text NOT NULL DEFAULT 'published' CHECK(publication_state IN ('published','retired')),
 prepared_at timestamptz NOT NULL DEFAULT clock_timestamp(), published_at timestamptz NOT NULL DEFAULT clock_timestamp(), retired_at timestamptz,
 UNIQUE(object_id,content_generation,source_seq,profile_revision), UNIQUE(object_id,content_generation,id),
 FOREIGN KEY(object_id,content_generation) REFERENCES ai_objects
);
CREATE TABLE ai_source_blocks (
 source_id uuid NOT NULL REFERENCES ai_sources(id), ordinal int NOT NULL CHECK(ordinal>=0), block_key text NOT NULL,
 text text NOT NULL, kind text NOT NULL, heading_path text NOT NULL DEFAULT '',
 location jsonb NOT NULL DEFAULT '{}', source_start bigint, source_end bigint, offset_unit text NOT NULL DEFAULT 'unknown',
 location_precision text NOT NULL DEFAULT 'block', source_kind text NOT NULL DEFAULT 'document', encoding text NOT NULL DEFAULT '',
 merged_ranges jsonb NOT NULL DEFAULT '[]', PRIMARY KEY(source_id,ordinal), UNIQUE(source_id,block_key)
);
CREATE TABLE ai_content_versions (
 id uuid PRIMARY KEY, source_id uuid NOT NULL, object_id uuid NOT NULL, content_generation bigint NOT NULL,
 text_seq bigint NOT NULL, chunker_version text NOT NULL, tokenizer_id text NOT NULL, tokenizer_version text NOT NULL,
 config_digest text NOT NULL, parent_count int NOT NULL CHECK(parent_count>=0), child_count int NOT NULL CHECK(child_count>=0), text_hash text NOT NULL,
 coverage text NOT NULL CHECK(coverage IN ('complete','partial','none')), truncated boolean NOT NULL DEFAULT false, warnings jsonb NOT NULL DEFAULT '[]',
 publication_state text NOT NULL DEFAULT 'published' CHECK(publication_state IN ('published','retired')),
 published_at timestamptz NOT NULL DEFAULT clock_timestamp(), retired_at timestamptz,
 UNIQUE(source_id,text_seq,config_digest), UNIQUE(object_id,content_generation,id),
 FOREIGN KEY(object_id,content_generation,source_id) REFERENCES ai_sources(object_id,content_generation,id)
);
CREATE TABLE ai_parent_chunks (
 id uuid PRIMARY KEY, content_version_id uuid NOT NULL REFERENCES ai_content_versions(id), ordinal int NOT NULL,
 parent_key text NOT NULL, content text NOT NULL, heading_path text NOT NULL DEFAULT '', kind text NOT NULL,
 block_start int NOT NULL, block_end int NOT NULL, location jsonb NOT NULL DEFAULT '{}',
 token_count int NOT NULL CHECK(token_count>0), token_count_is_estimate boolean NOT NULL DEFAULT true,
 UNIQUE(content_version_id,ordinal), UNIQUE(content_version_id,id)
);
CREATE TABLE ai_child_chunks (
 id uuid PRIMARY KEY, content_version_id uuid NOT NULL REFERENCES ai_content_versions(id), parent_id uuid NOT NULL,
 ordinal int NOT NULL, child_key text NOT NULL, content text NOT NULL, heading_path text NOT NULL DEFAULT '',
 location jsonb NOT NULL DEFAULT '{}', block_start int NOT NULL, block_end int NOT NULL,
 parent_start int NOT NULL CHECK(parent_start>=0), parent_end int NOT NULL CHECK(parent_end>parent_start),
 location_precision text NOT NULL DEFAULT 'block', token_count int NOT NULL CHECK(token_count>0), token_count_is_estimate boolean NOT NULL DEFAULT true,
 content_tsv tsvector NOT NULL DEFAULT '',
 UNIQUE(content_version_id,ordinal), UNIQUE(content_version_id,id),
 FOREIGN KEY(content_version_id,parent_id) REFERENCES ai_parent_chunks(content_version_id,id)
);
CREATE TABLE ai_embedding_sets (
 id uuid PRIMARY KEY, content_version_id uuid NOT NULL, object_id uuid NOT NULL, content_generation bigint NOT NULL, vector_seq bigint NOT NULL,
 provider text NOT NULL, model text NOT NULL, revision text NOT NULL, dimension int NOT NULL CHECK(dimension=1024),
 distance_metric text NOT NULL DEFAULT 'cosine', normalization text NOT NULL, input_digest text NOT NULL,
 vector_count int NOT NULL CHECK(vector_count>0), publication_state text NOT NULL DEFAULT 'published',
 published_at timestamptz NOT NULL DEFAULT clock_timestamp(), retired_at timestamptz,
 UNIQUE(content_version_id,vector_seq,input_digest), UNIQUE(content_version_id,id), UNIQUE(object_id,content_generation,content_version_id,id),
 FOREIGN KEY(object_id,content_generation,content_version_id) REFERENCES ai_content_versions(object_id,content_generation,id)
);
CREATE TABLE ai_chunk_vectors (
 embedding_set_id uuid NOT NULL, child_id uuid NOT NULL, content_version_id uuid NOT NULL,
 embedding vector(1024) NOT NULL, input_text_hash text NOT NULL,
 PRIMARY KEY(embedding_set_id,child_id),
 FOREIGN KEY(content_version_id,embedding_set_id) REFERENCES ai_embedding_sets(content_version_id,id),
 FOREIGN KEY(content_version_id,child_id) REFERENCES ai_child_chunks(content_version_id,id)
);
CREATE TABLE ai_summaries (
 id uuid PRIMARY KEY, source_id uuid NOT NULL, object_id uuid NOT NULL, content_generation bigint NOT NULL, summary_seq bigint NOT NULL,
 provider text NOT NULL, chat_model text NOT NULL, revision text NOT NULL, prompt_version text NOT NULL, strategy_version text NOT NULL,
 input_digest text NOT NULL, summary text NOT NULL, tags jsonb NOT NULL DEFAULT '[]', language text NOT NULL,
 coverage text NOT NULL CHECK(coverage IN ('complete','partial','none')), source_block_count int NOT NULL, processed_block_count int NOT NULL,
 truncated boolean NOT NULL DEFAULT false, warnings jsonb NOT NULL DEFAULT '[]', publication_state text NOT NULL DEFAULT 'published',
 published_at timestamptz NOT NULL DEFAULT clock_timestamp(), retired_at timestamptz,
 UNIQUE(source_id,summary_seq,input_digest), UNIQUE(object_id,content_generation,id),
 FOREIGN KEY(object_id,content_generation,source_id) REFERENCES ai_sources(object_id,content_generation,id),
 CHECK(processed_block_count>=0 AND processed_block_count<=source_block_count),
 CHECK(coverage<>'complete' OR (processed_block_count=source_block_count AND NOT truncated))
);
ALTER TABLE ai_objects ADD CONSTRAINT ai_active_source FOREIGN KEY(object_id,content_generation,active_source_id) REFERENCES ai_sources(object_id,content_generation,id);
ALTER TABLE ai_objects ADD CONSTRAINT ai_active_content FOREIGN KEY(object_id,content_generation,active_content_version_id) REFERENCES ai_content_versions(object_id,content_generation,id);
ALTER TABLE ai_objects ADD CONSTRAINT ai_active_vectors FOREIGN KEY(object_id,content_generation,active_content_version_id,active_embedding_set_id) REFERENCES ai_embedding_sets(object_id,content_generation,content_version_id,id);
ALTER TABLE ai_objects ADD CONSTRAINT ai_active_summary FOREIGN KEY(object_id,content_generation,active_summary_id) REFERENCES ai_summaries(object_id,content_generation,id);
CREATE TABLE ai_artifact_manifests (
 artifact_id uuid PRIMARY KEY, task_id uuid NOT NULL, generation bigint NOT NULL, kind text NOT NULL,
 input_digest text NOT NULL, artifact_hash text NOT NULL, typed_output_ref uuid NOT NULL,
 row_count bigint NOT NULL CHECK(row_count>=0), byte_count bigint NOT NULL CHECK(byte_count>=0),
 complete boolean NOT NULL CHECK(complete), data jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), UNIQUE(task_id,generation,kind)
);
CREATE TABLE ai_commit_receipts (
 result_id uuid PRIMARY KEY, task_id uuid NOT NULL, generation bigint NOT NULL, input_digest text NOT NULL,
 result_digest text NOT NULL, artifact_hash text NOT NULL, effect_kind text NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('succeeded','superseded','rejected','failed')),
 published_refs jsonb NOT NULL DEFAULT '{}', revision bigint NOT NULL DEFAULT 1, committed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE ai_capability_status (
 object_id uuid NOT NULL, content_generation bigint NOT NULL, capability text NOT NULL CHECK(capability IN ('source','text','vector','summary')),
 desired_seq bigint NOT NULL, latest_observed_seq bigint NOT NULL, latest_build_state text NOT NULL,
 error_code text NOT NULL DEFAULT '', safe_error_message text NOT NULL DEFAULT '', source_task_id uuid NOT NULL,
 observed_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(object_id,content_generation,capability),
 FOREIGN KEY(object_id,content_generation) REFERENCES ai_objects
);
CREATE TABLE ai_projection_targets (
 id uuid PRIMARY KEY, backend text NOT NULL CHECK(backend IN ('es','qd')), projection_kind text NOT NULL,
 target_version text NOT NULL, index_epoch bigint NOT NULL, config_digest text NOT NULL,
 state text NOT NULL CHECK(state IN ('building','active','retired','failed')), activated_at timestamptz,
 CHECK((backend='es' AND projection_kind='text') OR (backend='qd' AND projection_kind='vector'))
);
CREATE UNIQUE INDEX ai_projection_targets_active ON ai_projection_targets(backend,projection_kind) WHERE state='active';
CREATE TABLE ai_projection_states (
 backend text NOT NULL, target_id uuid NOT NULL REFERENCES ai_projection_targets(id), object_id uuid NOT NULL, content_generation bigint NOT NULL,
 content_version_id uuid NOT NULL, embedding_set_id uuid, artifact_version_id uuid NOT NULL,
 operation text NOT NULL CHECK(operation IN ('upsert','delete')), source_revision bigint NOT NULL, artifact_hash text NOT NULL,
 expected_count int NOT NULL, applied_count int NOT NULL, state text NOT NULL, last_error text NOT NULL DEFAULT '', published_at timestamptz,
 PRIMARY KEY(target_id,artifact_version_id,operation), FOREIGN KEY(object_id,content_generation) REFERENCES ai_objects,
 CHECK((backend='es' AND embedding_set_id IS NULL AND artifact_version_id=content_version_id) OR (backend='qd' AND embedding_set_id IS NOT NULL AND artifact_version_id=embedding_set_id)),
 CHECK(state<>'complete' OR expected_count=applied_count)
);
CREATE TABLE ai_migration_runs (
 id uuid PRIMARY KEY, source_identity text NOT NULL, source_revision text NOT NULL, boundary jsonb NOT NULL, scope_hash text NOT NULL,
 stage text NOT NULL, cursor text NOT NULL DEFAULT '', manifest jsonb NOT NULL DEFAULT '{}',
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(), completed_at timestamptz
);
CREATE TABLE ai_migration_items (
 run_id uuid NOT NULL REFERENCES ai_migration_runs(id), object_id uuid NOT NULL, content_generation bigint NOT NULL,
 source_hash text NOT NULL, target_id uuid, target_hash text NOT NULL DEFAULT '', conclusion text NOT NULL,
 details jsonb NOT NULL DEFAULT '{}', updated_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(run_id,object_id,content_generation)
);
-- Only these views are readable by online query/compute accounts. Staging is
-- deliberately a separate immutable table; publication inserts normalized rows.
CREATE VIEW ai_published_objects AS SELECT * FROM ai_objects WHERE tombstoned_at IS NULL;
CREATE VIEW ai_published_sources AS SELECT s.* FROM ai_sources s JOIN ai_objects o USING(object_id,content_generation) WHERE o.tombstoned_at IS NULL AND s.publication_state='published';
CREATE VIEW ai_published_blocks AS SELECT b.* FROM ai_source_blocks b JOIN ai_published_sources s ON s.id=b.source_id;
CREATE VIEW ai_published_content AS SELECT c.* FROM ai_content_versions c JOIN ai_objects o USING(object_id,content_generation) WHERE o.tombstoned_at IS NULL AND c.publication_state='published';
CREATE VIEW ai_published_parents AS SELECT p.* FROM ai_parent_chunks p JOIN ai_published_content c ON c.id=p.content_version_id;
CREATE VIEW ai_published_children AS SELECT p.* FROM ai_child_chunks p JOIN ai_published_content c ON c.id=p.content_version_id;
CREATE VIEW ai_published_sets AS SELECT s.* FROM ai_embedding_sets s JOIN ai_objects o USING(object_id,content_generation) WHERE o.tombstoned_at IS NULL AND s.publication_state='published';
CREATE VIEW ai_published_vectors AS SELECT v.* FROM ai_chunk_vectors v JOIN ai_published_sets s ON s.id=v.embedding_set_id;
CREATE VIEW ai_published_summaries AS SELECT s.* FROM ai_summaries s JOIN ai_objects o USING(object_id,content_generation) WHERE o.tombstoned_at IS NULL AND s.publication_state='published';
CREATE VIEW ai_published_capabilities AS SELECT s.* FROM ai_capability_status s JOIN ai_published_objects o USING(object_id,content_generation);
CREATE VIEW ai_published_targets AS SELECT * FROM ai_projection_targets WHERE state='active';
CREATE VIEW ai_published_projections AS SELECT * FROM ai_projection_states WHERE state='complete';
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;
