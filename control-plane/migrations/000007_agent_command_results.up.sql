ALTER TABLE transport_events
    DROP CONSTRAINT transport_events_event_type_check,
    ADD CONSTRAINT transport_events_event_type_check CHECK (
        event_type IN ('connected', 'disconnected', 'command_result', 'heartbeat', 'error', 'path_changed', 'telemetry', 'simulation_result')
    );

ALTER TABLE commands
    DROP CONSTRAINT commands_state_check,
    ADD CONSTRAINT commands_state_check CHECK (
        state IN ('queued', 'dispatched', 'accepted', 'running', 'succeeded', 'failed', 'rejected', 'unknown', 'expired', 'rolled_back', 'superseded')
    );

CREATE TABLE agent_command_results (
    event_id uuid PRIMARY KEY REFERENCES transport_events(event_id) ON DELETE RESTRICT,
    command_id uuid NOT NULL REFERENCES commands(id) ON DELETE RESTRICT,
    idempotency_key uuid NOT NULL,
    payload_sha256 bytea CHECK (payload_sha256 IS NULL OR octet_length(payload_sha256) = 32),
    state text NOT NULL CHECK (state IN ('succeeded', 'failed', 'unknown', 'rejected')),
    result bytea NOT NULL CHECK (octet_length(result) <= 1048576),
    error_code text CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 128),
    accepted_at timestamptz,
    completed_at timestamptz NOT NULL,
    replayed boolean NOT NULL,
    created_at timestamptz NOT NULL,
    CHECK (accepted_at IS NULL OR accepted_at <= completed_at),
    CHECK (
        (state = 'succeeded' AND payload_sha256 IS NOT NULL AND accepted_at IS NOT NULL AND error_code IS NULL)
        OR (state = 'failed' AND payload_sha256 IS NOT NULL AND accepted_at IS NOT NULL AND error_code IS NOT NULL)
        OR (state = 'unknown' AND payload_sha256 IS NOT NULL AND accepted_at IS NOT NULL AND error_code IS NOT NULL AND octet_length(result) = 0)
        OR (state = 'rejected' AND accepted_at IS NULL AND error_code IS NOT NULL AND octet_length(result) = 0)
    )
);
CREATE INDEX agent_command_results_command_created_idx
    ON agent_command_results(command_id, created_at);

COMMENT ON TABLE agent_command_results IS 'Durable Agent journal results; unknown outcomes require observation before retry.';
