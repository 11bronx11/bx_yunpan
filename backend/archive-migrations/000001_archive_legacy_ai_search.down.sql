-- Restore schema positions and original business FK definitions. New AI output
-- is retained in the independent database; it is not reverse-converted.
DO $$ DECLARE item text; fk record;
 source_schema text := current_schema();
 archive_schema text := CASE WHEN current_schema()='public' THEN 'ai_legacy_20261116' ELSE current_schema()||'_ai_legacy_20261116' END;
 BEGIN
 FOREACH item IN ARRAY ARRAY['ai_documents','ai_document_versions','ai_chunks','search_index_versions','search_content_projections','search_file_projections','search_projection_checkpoints'] LOOP
  IF to_regclass(format('%I.%I',archive_schema,item)) IS NOT NULL THEN
   IF to_regclass(format('%I.%I',source_schema,item)) IS NOT NULL THEN RAISE EXCEPTION 'online table % already exists',item; END IF;
   EXECUTE format('ALTER TABLE %I.%I SET SCHEMA %I',archive_schema,item,source_schema);
  END IF;
 END LOOP;
 IF to_regclass(format('%I.archived_constraints',archive_schema)) IS NOT NULL THEN
  FOR fk IN EXECUTE format('SELECT * FROM %I.archived_constraints',archive_schema) LOOP
   EXECUTE format('ALTER TABLE %I.%I ADD CONSTRAINT %I %s',source_schema,fk.table_name,fk.constraint_name,fk.definition);
  END LOOP;
 END IF;
END $$;
