-- Restore heading + passage recall without changing exact parent/child ranges.
CREATE OR REPLACE FUNCTION ai_child_fts_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.content_tsv=ai_fts_text(CASE WHEN btrim(NEW.heading_path)='' OR NEW.content=btrim(NEW.heading_path) OR
  left(NEW.content,length(btrim(NEW.heading_path))+1)=btrim(NEW.heading_path)||E'\n'
  THEN NEW.content ELSE btrim(NEW.heading_path)||E'\n\n'||NEW.content END);
 RETURN NEW;
END $$;
DROP TRIGGER ai_child_fts ON ai_child_chunks;
CREATE TRIGGER ai_child_fts BEFORE INSERT OR UPDATE OF content,heading_path ON ai_child_chunks FOR EACH ROW EXECUTE FUNCTION ai_child_fts_update();
UPDATE ai_child_chunks SET content=content;
