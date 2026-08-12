ALTER TABLE audit_events
    ADD COLUMN auth_version smallint NOT NULL DEFAULT 0,
    ADD COLUMN event_key_id text,
    ADD COLUMN event_mac bytea,
    ADD CONSTRAINT audit_event_auth_version CHECK (auth_version IN (0, 1)),
    ADD CONSTRAINT audit_event_auth_fields CHECK (
        (auth_version = 0 AND event_key_id IS NULL AND event_mac IS NULL)
        OR
        (auth_version = 1
            AND length(event_key_id) BETWEEN 1 AND 128
            AND event_key_id ~ '^[A-Za-z0-9._-]+$'
            AND octet_length(event_mac) = 32)
    );

COMMENT ON COLUMN audit_events.auth_version IS
    'Version 0 marks checkpoint-anchored legacy rows; version 1 requires application-origin HMAC authentication.';
COMMENT ON COLUMN audit_events.event_key_id IS
    'Identifier of the Controller audit-event HMAC key; never contains key material.';
COMMENT ON COLUMN audit_events.event_mac IS
    'HMAC-SHA256 over the domain-separated event_hash using the Controller audit-event key.';
