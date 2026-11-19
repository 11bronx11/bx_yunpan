-- Protocol and effects are not reversibly convertible by a schema rollback.
DO $$ BEGIN RAISE EXCEPTION 'stop v2 writers and use the documented forward recovery; v2 handover cannot be undone by down migration'; END $$;
