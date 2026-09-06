BEGIN;

CREATE TABLE ai_document_versions (
    id uuid PRIMARY KEY,
    document_id uuid NOT NULL REFERENCES ai_documents(id) ON DELETE CASCADE,
    object_id uuid NOT NULL REFERENCES file_objects(id) ON DELETE CASCADE,
    pipeline_version varchar(64) NOT NULL,
    model_version varchar(128) NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    status varchar(32) NOT NULL CHECK (status IN ('pending', 'building', 'indexed', 'failed', 'unsupported')),
    summary text,
    tags text[] NOT NULL DEFAULT '{}',
    language varchar(32),
    error_code varchar(128),
    lease_token uuid,
    lease_until timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    indexed_at timestamptz,
    retired_at timestamptz,
    UNIQUE (object_id, pipeline_version, model_version, generation),
    UNIQUE (document_id, generation)
);

ALTER TABLE ai_documents ADD COLUMN active_version_id uuid;
ALTER TABLE ai_chunks ADD COLUMN document_version_id uuid;

-- Deterministic IDs make the expand backfill repeatable in an interrupted
-- migration and avoid fabricating a new generation on every retry.
INSERT INTO ai_document_versions (
    id, document_id, object_id, pipeline_version, model_version, generation,
    status, summary, tags, language, error_code, created_at, updated_at, indexed_at
)
SELECT
    md5(d.id::text || ':version:1')::uuid, d.id, d.object_id, d.pipeline_version,
    d.model_version, 1,
    CASE WHEN d.status = 'processing' THEN 'building' ELSE d.status END,
    d.summary, d.tags, d.language, d.error_code,
    d.created_at, d.updated_at,
    CASE WHEN d.status = 'indexed' THEN d.updated_at ELSE NULL END
FROM ai_documents d
ON CONFLICT (document_id, generation) DO NOTHING;

UPDATE ai_documents d
SET active_version_id = v.id
FROM ai_document_versions v
WHERE v.document_id = d.id
  AND v.generation = 1
  AND d.status = 'indexed'
  AND d.active_version_id IS NULL;

UPDATE ai_chunks c
SET document_version_id = md5(c.document_id::text || ':version:1')::uuid
WHERE c.document_version_id IS NULL;

ALTER TABLE ai_documents
    ADD CONSTRAINT ai_documents_active_version_fk
    FOREIGN KEY (active_version_id) REFERENCES ai_document_versions(id) ON DELETE SET NULL;
ALTER TABLE ai_chunks
    ADD CONSTRAINT ai_chunks_document_version_fk
    FOREIGN KEY (document_version_id) REFERENCES ai_document_versions(id) ON DELETE CASCADE;

-- The old key prevented two generations from coexisting. Keep the legacy
-- document_id column for dual-read compatibility, but make version identity
-- the uniqueness boundary for new writes.
ALTER TABLE ai_chunks DROP CONSTRAINT IF EXISTS ai_chunks_document_id_ordinal_key;
CREATE UNIQUE INDEX idx_ai_chunks_version_ordinal ON ai_chunks(document_version_id, ordinal);
CREATE INDEX idx_ai_document_versions_active ON ai_document_versions(document_id, status, generation DESC);
CREATE INDEX idx_ai_document_versions_lease ON ai_document_versions(status, lease_until, updated_at);

COMMIT;
