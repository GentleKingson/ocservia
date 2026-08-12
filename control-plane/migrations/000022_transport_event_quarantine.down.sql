DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM transport_event_quarantine) THEN
        RAISE EXCEPTION 'cannot remove transport quarantine while permanent-invalid evidence exists';
    END IF;
END;
$$;

DROP TABLE IF EXISTS transport_event_quarantine;
DROP TABLE IF EXISTS transport_event_cursor;
