DO $$ BEGIN
    RAISE EXCEPTION 'bounded telemetry retention migration is forward-only';
END $$;
