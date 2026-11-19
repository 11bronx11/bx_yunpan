-- A cross-database preflight in ai-direct-admin must set this transaction GUC.
DO $$ BEGIN
 IF current_setting('bx.ai_archive_verified',true) IS DISTINCT FROM 'true' THEN
  RAISE EXCEPTION 'use ai-direct-admin archive to verify control and AI evidence';
 END IF;
 IF EXISTS(SELECT 1 FROM ai_objects WHERE writer_protocol<>3) OR
    EXISTS(SELECT 1 FROM ai_migration_runs WHERE stage NOT IN ('verified','archived')) THEN
  RAISE EXCEPTION 'legacy AI objects or migration batches remain';
 END IF;
END $$;
CREATE SCHEMA ai_v2_archive;
REVOKE ALL ON SCHEMA ai_v2_archive FROM PUBLIC;
DROP VIEW ai_summary_checkpoints;
CREATE VIEW ai_summary_checkpoints AS SELECT * FROM ai_summary_nodes;
REVOKE ALL ON ai_summary_checkpoints FROM PUBLIC;
ALTER TABLE ai_artifact_manifests SET SCHEMA ai_v2_archive;
ALTER TABLE ai_migration_legacy_tasks SET SCHEMA ai_v2_archive;
ALTER TABLE ai_migration_items SET SCHEMA ai_v2_archive;
ALTER TABLE ai_migration_runs SET SCHEMA ai_v2_archive;
