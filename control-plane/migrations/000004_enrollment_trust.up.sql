ALTER TABLE nodes DROP CONSTRAINT nodes_status_check;
UPDATE nodes SET status = 'active' WHERE status = 'approved';
ALTER TABLE nodes
    ADD CONSTRAINT nodes_status_check
    CHECK (status IN ('pending', 'active', 'revoked', 'offline'));
ALTER TABLE nodes
    ADD COLUMN labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN policy text;

CREATE TABLE enrollment_tokens (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    expected_environment text NOT NULL CHECK (length(expected_environment) BETWEEN 1 AND 64),
    expected_node_name text CHECK (expected_node_name IS NULL OR length(expected_node_name) BETWEEN 1 AND 128),
    expected_endpoint_id bytea CHECK (expected_endpoint_id IS NULL OR octet_length(expected_endpoint_id) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    consumed_node_id uuid,
    created_by text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 256),
    created_at timestamptz NOT NULL,
    CHECK (expires_at > created_at),
    CHECK ((consumed_at IS NULL) = (consumed_node_id IS NULL)),
    FOREIGN KEY (workspace_id, consumed_node_id) REFERENCES nodes(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX enrollment_tokens_workspace_expiry_idx
    ON enrollment_tokens (workspace_id, expires_at DESC);

CREATE TABLE node_endpoint_keys (
    node_id uuid PRIMARY KEY REFERENCES nodes(id) ON DELETE RESTRICT,
    endpoint_id bytea NOT NULL UNIQUE CHECK (octet_length(endpoint_id) = 32),
    state text NOT NULL CHECK (state IN ('pending', 'active', 'revoked')),
    bound_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);

CREATE TABLE node_capabilities (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    capability text NOT NULL CHECK (length(capability) BETWEEN 1 AND 128),
    approved boolean NOT NULL DEFAULT false,
    PRIMARY KEY (node_id, capability)
);

COMMENT ON COLUMN enrollment_tokens.token_hash IS
    'SHA-256 digest only; plaintext enrollment tokens must never be persisted.';
