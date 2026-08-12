CREATE TABLE transport_event_cursor (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    event_id uuid NOT NULL,
    valid boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL
);

INSERT INTO transport_event_cursor (singleton, event_id, valid, updated_at)
SELECT true, event_id, true, received_at
FROM transport_events
WHERE transport_cursor_valid
ORDER BY ingest_sequence DESC
LIMIT 1;

CREATE TABLE transport_event_quarantine (
    event_id uuid PRIMARY KEY,
    node_id uuid NOT NULL,
    event_type integer NOT NULL,
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
    reason_code text NOT NULL CHECK (reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    reason_detail text NOT NULL CHECK (octet_length(reason_detail) BETWEEN 1 AND 256),
    observed_at timestamptz NOT NULL
);
CREATE INDEX transport_event_quarantine_node_time_idx
    ON transport_event_quarantine (node_id, observed_at DESC);

COMMENT ON TABLE transport_event_cursor IS 'Durable cursor for both accepted and quarantined transport events.';
COMMENT ON TABLE transport_event_quarantine IS 'Bounded metadata for permanently invalid transport events; raw attacker payloads are never retained.';
