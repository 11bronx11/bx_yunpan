CREATE OR REPLACE FUNCTION ai_child_fts_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.content_tsv=ai_fts_text(NEW.content);
 RETURN NEW;
END $$;
DROP TRIGGER ai_child_fts ON ai_child_chunks;
CREATE TRIGGER ai_child_fts BEFORE INSERT OR UPDATE OF content ON ai_child_chunks FOR EACH ROW EXECUTE FUNCTION ai_child_fts_update();
UPDATE ai_child_chunks SET content=content;
