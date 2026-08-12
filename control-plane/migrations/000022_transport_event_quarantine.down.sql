BEGIN;

LOCK TABLE transport_event_cursor, transport_event_quarantine, transport_events
    IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    durable_cursor uuid;
    legacy_cursor uuid;
BEGIN
    IF EXISTS (SELECT 1 FROM transport_event_quarantine) THEN
        RAISE EXCEPTION 'cannot remove transport quarantine while permanent-invalid evidence exists';
    END IF;

    SELECT event_id
    INTO durable_cursor
    FROM transport_event_cursor
    WHERE singleton AND valid;

    SELECT event_id
    INTO legacy_cursor
    FROM transport_events
    WHERE transport_cursor_valid
    ORDER BY ingest_sequence DESC
    LIMIT 1;

    IF durable_cursor IS DISTINCT FROM legacy_cursor THEN
        RAISE EXCEPTION 'durable transport cursor cannot be represented by the legacy cursor';
    END IF;
END;
$$;

DROP TABLE IF EXISTS transport_event_quarantine;
DROP TABLE IF EXISTS transport_event_cursor;

COMMIT;
