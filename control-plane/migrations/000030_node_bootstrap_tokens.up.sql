CREATE TABLE node_bootstrap_tokens (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    expected_environment text NOT NULL CHECK (length(expected_environment) BETWEEN 1 AND 64),
    expected_node_name text CHECK (expected_node_name IS NULL OR length(expected_node_name) BETWEEN 1 AND 128),
    expires_at timestamptz NOT NULL,
    bound_endpoint_id bytea CHECK (bound_endpoint_id IS NULL OR octet_length(bound_endpoint_id) = 32),
    consumed_node_id uuid,
    consumed_at timestamptz,
    created_by text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 256),
    created_at timestamptz NOT NULL,
    CHECK (expires_at > created_at),
    CHECK ((bound_endpoint_id IS NULL) = (consumed_at IS NULL)),
    CHECK ((consumed_at IS NULL) = (consumed_node_id IS NULL)),
    FOREIGN KEY (workspace_id, consumed_node_id) REFERENCES nodes(workspace_id, id) ON DELETE RESTRICT
);

CREATE INDEX node_bootstrap_tokens_workspace_expiry_idx
    ON node_bootstrap_tokens (workspace_id, expires_at DESC);

COMMENT ON COLUMN node_bootstrap_tokens.token_hash IS
    'SHA-256 digest only; plaintext node bootstrap tokens must never be persisted.';

-- This additive table is invisible to version 29 Controllers, so they remain
-- safe during a rolling Controller upgrade.
UPDATE controller_schema_compatibility
SET minimum_compatible_controller_schema = 29
WHERE singleton AND "current_schema" = 30;
