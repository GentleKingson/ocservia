CREATE TABLE agent_command_results (
    event_id uuid PRIMARY KEY REFERENCES transport_events(event_id) ON DELETE RESTRICT,
    command_id uuid NOT NULL REFERENCES commands(id) ON DELETE RESTRICT,
    idempotency_key uuid NOT NULL,
    payload_sha256 bytea CHECK (payload_sha256 IS NULL OR octet_length(payload_sha256) = 32),
    state text NOT NULL CHECK (state IN ('succeeded', 'failed', 'unknown')),
    result bytea NOT NULL CHECK (octet_length(result) <= 1048576),
    error_code text CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 128),
    accepted_at timestamptz,
    completed_at timestamptz NOT NULL,
    replayed boolean NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX agent_command_results_command_created_idx
    ON agent_command_results(command_id, created_at);

COMMENT ON TABLE agent_command_results IS 'Durable Agent journal results; unknown outcomes require observation before retry.';
