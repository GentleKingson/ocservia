ALTER TABLE operations
    ADD COLUMN idempotency_key text,
    ADD COLUMN request_hash bytea,
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN completed_at timestamptz,
    ADD CONSTRAINT operations_idempotency_pair CHECK (
        (idempotency_key IS NULL) = (request_hash IS NULL)
    ),
    ADD CONSTRAINT operations_request_hash_size CHECK (
        request_hash IS NULL OR octet_length(request_hash) = 32
    );

CREATE UNIQUE INDEX operations_workspace_idempotency_idx
    ON operations (workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE commands (
    id uuid PRIMARY KEY,
    operation_id uuid NOT NULL UNIQUE REFERENCES operations(id) ON DELETE RESTRICT,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid NOT NULL,
    state text NOT NULL CHECK (state IN ('queued', 'dispatched', 'accepted', 'running', 'succeeded', 'failed', 'unknown', 'expired', 'rolled_back', 'superseded')),
    payload_type text NOT NULL CHECK (payload_type IN ('synthetic_noop', 'synthetic_echo')),
    envelope bytea NOT NULL CHECK (octet_length(envelope) BETWEEN 1 AND 1048576),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    expected_version bigint NOT NULL CHECK (expected_version > 0),
    sequence bigint NOT NULL DEFAULT 1 CHECK (sequence > 0),
    traceparent text NOT NULL CHECK (traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (workspace_id, node_id) REFERENCES nodes(workspace_id, id) ON DELETE RESTRICT,
    UNIQUE (workspace_id, idempotency_key),
    CHECK (expires_at > created_at)
);
CREATE INDEX commands_node_state_idx ON commands (node_id, state, created_at, id);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY,
    command_id uuid NOT NULL UNIQUE REFERENCES commands(id) ON DELETE RESTRICT,
    event_type text NOT NULL CHECK (event_type = 'command.dispatch'),
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 1048576),
    available_at timestamptz NOT NULL,
    locked_by uuid,
    locked_until timestamptz,
    published_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text,
    created_at timestamptz NOT NULL,
    CHECK ((locked_by IS NULL) = (locked_until IS NULL)),
    CHECK (published_at IS NULL OR locked_by IS NULL)
);
CREATE INDEX outbox_events_dispatch_idx
    ON outbox_events (available_at, id)
    WHERE published_at IS NULL;

CREATE TABLE command_attempts (
    id uuid PRIMARY KEY,
    command_id uuid NOT NULL REFERENCES commands(id) ON DELETE RESTRICT,
    outbox_event_id uuid NOT NULL REFERENCES outbox_events(id) ON DELETE RESTRICT,
    worker_id uuid NOT NULL,
    attempt_number integer NOT NULL CHECK (attempt_number > 0),
    state text NOT NULL CHECK (state IN ('sending', 'sent', 'failed', 'unknown')),
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    error_code text,
    UNIQUE (command_id, attempt_number),
    CHECK ((state = 'sending') = (finished_at IS NULL))
);

CREATE TABLE node_command_leases (
    node_id uuid PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    command_id uuid NOT NULL UNIQUE REFERENCES commands(id) ON DELETE CASCADE,
    lease_token uuid NOT NULL UNIQUE,
    worker_id uuid NOT NULL,
    leased_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX node_command_leases_expiry_idx ON node_command_leases (leased_until);

CREATE TABLE operation_events (
    sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id uuid NOT NULL UNIQUE,
    operation_id uuid NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    state text NOT NULL CHECK (state IN ('queued', 'dispatched', 'accepted', 'running', 'succeeded', 'failed', 'unknown', 'expired', 'rolled_back', 'superseded')),
    occurred_at timestamptz NOT NULL
);
CREATE INDEX operation_events_operation_sequence_idx
    ON operation_events (operation_id, sequence);

COMMENT ON TABLE outbox_events IS 'Transactional outbox; delivery correctness never depends on LISTEN/NOTIFY.';
COMMENT ON TABLE command_attempts IS 'Immutable dispatch-attempt history; sending attempts become unknown when their lease expires.';
COMMENT ON COLUMN commands.envelope IS 'Serialized typed CommandEnvelope protobuf; arbitrary JSON and method strings are forbidden.';
