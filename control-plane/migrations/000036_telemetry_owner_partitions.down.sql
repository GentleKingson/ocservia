DO $$ BEGIN RAISE EXCEPTION 'telemetry runtime boundary migration is forward-only'; END $$;
