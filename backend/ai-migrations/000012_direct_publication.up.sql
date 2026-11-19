ALTER TABLE ai_objects ADD COLUMN writer_protocol integer NOT NULL DEFAULT 2 CHECK(writer_protocol IN (2,3));
ALTER TABLE ai_commit_receipts ADD COLUMN protocol_version integer NOT NULL DEFAULT 2;
ALTER TABLE ai_commit_receipts ADD COLUMN object_id uuid;
ALTER TABLE ai_commit_receipts ADD COLUMN content_generation bigint;
ALTER TABLE ai_commit_receipts ADD COLUMN contract_version integer NOT NULL DEFAULT 1;
CREATE UNIQUE INDEX ai_receipt_v3_task ON ai_commit_receipts(task_id) WHERE protocol_version=3;
CREATE INDEX ai_receipt_object ON ai_commit_receipts(object_id,content_generation,committed_at);
CREATE TABLE ai_summary_nodes (
 node_id uuid PRIMARY KEY, task_id uuid NOT NULL, input_digest text NOT NULL,
 object_id uuid NOT NULL, content_generation bigint NOT NULL, source_id uuid NOT NULL,
 profile_digest text NOT NULL, algorithm text NOT NULL, level integer NOT NULL, position integer NOT NULL,
 input_hash text NOT NULL, data jsonb NOT NULL, data_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(octet_length(data::text)<=1048576)
);
CREATE INDEX ai_summary_nodes_task ON ai_summary_nodes(task_id,created_at);
ALTER TABLE ai_orphan_projections ADD COLUMN task_id uuid;
ALTER TABLE ai_orphan_projections ADD COLUMN input_digest text NOT NULL DEFAULT '';
ALTER TABLE ai_orphan_projections ADD COLUMN writer_deadline timestamptz;
-- Old published orphan view stays unchanged: online query needs no private proof.
CREATE VIEW ai_direct_summary_checkpoints AS SELECT * FROM ai_summary_nodes;

CREATE FUNCTION fence_ai_object_writer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.writer_protocol=3 OR (TG_OP='UPDATE' AND OLD.writer_protocol=3))
 AND current_setting('bx.ai_protocol',true) IS DISTINCT FROM 'v3' THEN
  RAISE EXCEPTION 'AI object requires direct protocol writer';
 END IF;
 IF TG_OP='UPDATE' AND OLD.writer_protocol=3 AND NEW.writer_protocol<>3 THEN
  RAISE EXCEPTION 'AI protocol cannot be downgraded';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_object_writer BEFORE INSERT OR UPDATE ON ai_objects FOR EACH ROW EXECUTE FUNCTION fence_ai_object_writer();
-- Every publication locks/updates the object first; this additional fence also
-- prevents old normalized writes before their eventual rejected pointer update.
CREATE FUNCTION fence_ai_content_writer() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE row_data jsonb := to_jsonb(NEW); direct boolean;
BEGIN
 SELECT writer_protocol=3 INTO direct FROM ai_objects
 WHERE object_id=(row_data->>'object_id')::uuid AND content_generation=(row_data->>'content_generation')::bigint;
 IF direct AND current_setting('bx.ai_protocol',true) IS DISTINCT FROM 'v3' THEN
  RAISE EXCEPTION 'AI content requires direct protocol writer';
 END IF;
 RETURN NEW;
END $$;
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['ai_sources','ai_content_versions','ai_embedding_sets','ai_summaries','ai_capability_status','ai_projection_states','ai_orphan_projections'] LOOP
  EXECUTE format('CREATE TRIGGER ai_content_writer BEFORE INSERT OR UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION fence_ai_content_writer()',name);
 END LOOP;
END $$;
REVOKE ALL ON ai_summary_nodes,ai_direct_summary_checkpoints FROM PUBLIC;
