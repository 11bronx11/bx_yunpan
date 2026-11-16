-- Physical writes whose control result was never published still need bounded
-- deletion. These are cleanup facts, never a second execution scheduler.
CREATE TABLE ai_orphan_projections (
 backend text NOT NULL CHECK(backend IN ('es','qd')), target_id uuid NOT NULL,
 target_version text NOT NULL, object_id uuid NOT NULL, content_generation bigint NOT NULL,
 artifact_version_id uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(backend,target_id,artifact_version_id)
);
CREATE VIEW ai_published_orphan_projections AS SELECT * FROM ai_orphan_projections;
