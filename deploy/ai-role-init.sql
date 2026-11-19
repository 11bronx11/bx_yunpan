\set ON_ERROR_STOP on
\getenv query_password AI_QUERY_PASSWORD
\getenv compute_password AI_COMPUTE_PASSWORD
\getenv commit_password AI_COMMIT_PASSWORD
SELECT 'CREATE ROLE yunpan_ai_query LOGIN' WHERE NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='yunpan_ai_query') \gexec
SELECT 'CREATE ROLE yunpan_ai_compute LOGIN' WHERE NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='yunpan_ai_compute') \gexec
SELECT 'CREATE ROLE yunpan_ai_commit LOGIN' WHERE NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='yunpan_ai_commit') \gexec
ALTER ROLE yunpan_ai_query PASSWORD :'query_password';
ALTER ROLE yunpan_ai_compute PASSWORD :'compute_password';
ALTER ROLE yunpan_ai_commit PASSWORD :'commit_password';
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO yunpan_ai_query,yunpan_ai_compute,yunpan_ai_commit;
GRANT SELECT ON ai_published_objects,ai_published_sources,ai_published_blocks,ai_published_content,ai_published_parents,ai_published_children,ai_published_sets,ai_published_vectors,ai_published_summaries,ai_published_capabilities,ai_published_targets,ai_published_projections TO yunpan_ai_query,yunpan_ai_compute;
GRANT INSERT ON ai_artifact_manifests TO yunpan_ai_compute;
GRANT SELECT ON ai_summary_checkpoints TO yunpan_ai_compute;
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO yunpan_ai_commit;
-- Query stays restricted. Background v2 workers use the existing commit role
-- for staging and domain-local publication; the old compute role is retired.
REVOKE ALL ON ai_artifact_manifests FROM yunpan_ai_query;
REVOKE ALL ON ai_summary_checkpoints FROM yunpan_ai_query;
REVOKE UPDATE,DELETE ON ai_artifact_manifests FROM yunpan_ai_compute;
ALTER ROLE yunpan_ai_query SET default_transaction_read_only=on;

GRANT SELECT ON ai_projection_catalog TO yunpan_ai_compute;
GRANT SELECT ON ai_published_orphan_projections TO yunpan_ai_query,yunpan_ai_compute;
