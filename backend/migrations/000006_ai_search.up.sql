BEGIN;

CREATE TABLE ai_documents (
    id uuid PRIMARY KEY,
    object_id uuid NOT NULL UNIQUE REFERENCES file_objects(id) ON DELETE CASCADE,
    status varchar(32) NOT NULL CHECK (status IN ('pending', 'processing', 'indexed', 'failed', 'unsupported')),
    summary text,
    tags text[] NOT NULL DEFAULT '{}',
    language varchar(32),
    pipeline_version varchar(64) NOT NULL,
    model_version varchar(128) NOT NULL,
    error_code varchar(128),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ai_chunks (
    id uuid PRIMARY KEY,
    document_id uuid NOT NULL REFERENCES ai_documents(id) ON DELETE CASCADE,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    page_number integer,
    section varchar(255),
    content text NOT NULL,
    content_tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,
    embedding vector(1024) NOT NULL,
    token_count integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, ordinal)
);

CREATE INDEX idx_ai_chunks_document ON ai_chunks(document_id, ordinal);
CREATE INDEX idx_ai_chunks_fts ON ai_chunks USING gin(content_tsv);
CREATE INDEX idx_ai_chunks_embedding ON ai_chunks USING hnsw (embedding vector_cosine_ops);

COMMIT;
