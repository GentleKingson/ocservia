DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM audit_events WHERE auth_version = 1) THEN
        RAISE EXCEPTION 'cannot remove audit event authentication while authenticated audit history exists';
    END IF;
END
$$;

ALTER TABLE audit_events
    DROP CONSTRAINT IF EXISTS audit_event_auth_fields,
    DROP CONSTRAINT IF EXISTS audit_event_auth_version,
    DROP COLUMN IF EXISTS event_mac,
    DROP COLUMN IF EXISTS event_key_id,
    DROP COLUMN IF EXISTS auth_version;
