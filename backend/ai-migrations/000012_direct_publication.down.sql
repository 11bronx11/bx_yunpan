DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM ai_objects WHERE writer_protocol=3) OR EXISTS(SELECT 1 FROM ai_commit_receipts WHERE protocol_version=3) OR EXISTS(SELECT 1 FROM ai_summary_nodes) THEN
  RAISE EXCEPTION 'direct AI data requires forward repair';
 END IF;
END $$;
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['ai_sources','ai_content_versions','ai_embedding_sets','ai_summaries','ai_capability_status','ai_projection_states','ai_orphan_projections'] LOOP
  EXECUTE format('DROP TRIGGER ai_content_writer ON %I',name);
 END LOOP;
END $$;
DROP FUNCTION fence_ai_content_writer();
DROP TRIGGER ai_object_writer ON ai_objects;
DROP FUNCTION fence_ai_object_writer();
DROP VIEW ai_direct_summary_checkpoints;
DROP TABLE ai_summary_nodes;
ALTER TABLE ai_orphan_projections DROP COLUMN task_id, DROP COLUMN input_digest, DROP COLUMN writer_deadline;
DROP INDEX ai_receipt_v3_task;
DROP INDEX ai_receipt_object;
ALTER TABLE ai_commit_receipts DROP COLUMN protocol_version, DROP COLUMN object_id, DROP COLUMN content_generation, DROP COLUMN contract_version;
ALTER TABLE ai_objects DROP COLUMN writer_protocol;
