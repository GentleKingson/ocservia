-- Forward-only security boundary: restore a reviewed backup instead of running
-- an old Controller against a database with R4 lifecycle state.
DO $$ BEGIN RAISE EXCEPTION 'Local initialization migration is forward-only'; END $$;
