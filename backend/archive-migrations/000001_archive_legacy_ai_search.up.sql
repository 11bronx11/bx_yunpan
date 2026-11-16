-- Explicit archive stage, after the durable manifest has passed cutover-check.
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM ai_split_cutovers WHERE status='cutover_ready') THEN
  RAISE EXCEPTION 'ai-migrate cutover-check must record a verified manifest before archive';
 END IF;
END $$;
DO $$ DECLARE item text; fk record;
 source_schema text := current_schema();
 archive_schema text := CASE WHEN current_schema()='public' THEN 'ai_legacy_20261116' ELSE current_schema()||'_ai_legacy_20261116' END;
 legacy_tables text[] := ARRAY['ai_documents','ai_document_versions','ai_chunks','search_index_versions','search_content_projections','search_file_projections','search_projection_checkpoints'];
 BEGIN
 EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I',archive_schema);
 EXECUTE format('CREATE TABLE IF NOT EXISTS %I.archived_constraints(table_name text, constraint_name text, definition text, PRIMARY KEY(table_name,constraint_name))',archive_schema);
 FOREACH item IN ARRAY legacy_tables LOOP
  IF to_regclass(format('%I.%I',source_schema,item)) IS NOT NULL THEN
   FOR fk IN SELECT c.conname,pg_get_constraintdef(c.oid) AS definition FROM pg_constraint c WHERE c.conrelid=to_regclass(format('%I.%I',source_schema,item)) AND c.contype='f' AND c.confrelid NOT IN (SELECT t.oid FROM pg_class t JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname IN (source_schema,archive_schema) AND t.relname=ANY(legacy_tables)) LOOP
    EXECUTE format('INSERT INTO %I.archived_constraints VALUES($1,$2,$3) ON CONFLICT DO NOTHING',archive_schema) USING item,fk.conname,fk.definition;
    EXECUTE format('ALTER TABLE %I.%I DROP CONSTRAINT %I',source_schema,item,fk.conname);
   END LOOP;
   EXECUTE format('ALTER TABLE %I.%I SET SCHEMA %I',source_schema,item,archive_schema);
  END IF;
 END LOOP;
 EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC',archive_schema);
 EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA %I FROM PUBLIC',archive_schema);
END $$;
