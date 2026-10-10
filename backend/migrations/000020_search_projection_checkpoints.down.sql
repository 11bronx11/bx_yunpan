BEGIN;
DROP INDEX IF EXISTS idx_search_projection_checkpoints_state;
DROP TABLE IF EXISTS search_projection_checkpoints;
COMMIT;
