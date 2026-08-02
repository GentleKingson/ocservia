CREATE TABLE local_slice_jobs (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    command_envelope bytea NOT NULL CHECK (octet_length(command_envelope) BETWEEN 1 AND 1048576),
    traceparent text NOT NULL CHECK (traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at timestamptz NOT NULL,
    dispatched_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL
);
CREATE INDEX local_slice_jobs_dispatch_idx
    ON local_slice_jobs (available_at, operation_id)
    WHERE dispatched_at IS NULL;

CREATE TABLE transport_events (
    event_id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    event_type text NOT NULL CHECK (event_type IN ('connected', 'disconnected', 'command_result', 'heartbeat', 'error')),
    occurred_at timestamptz NOT NULL,
    traceparent text NOT NULL CHECK (traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    payload bytea NOT NULL CHECK (octet_length(payload) <= 1048576),
    transport_cursor_valid boolean NOT NULL DEFAULT true,
    received_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE local_slice_jobs IS 'Development-only I03 simulator dispatch queue; it has no remote side effects.';
COMMENT ON TABLE transport_events IS 'Idempotently ingested typed transport events used by REST and SSE rebuilds.';
