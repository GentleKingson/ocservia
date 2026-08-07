ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop', 'synthetic_echo', 'session_disconnect', 'session_terminate', 'ip_ban_remove', 'service_reload', 'user_create', 'user_disable', 'user_enable', 'user_password_rotate', 'group_apply', 'config_plan')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Only typed command payloads are dispatchable; raw shell, occtl, and systemctl operations are forbidden.';

CREATE TABLE node_config_state (
    node_id uuid PRIMARY KEY REFERENCES nodes(id) ON DELETE RESTRICT,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    candidate_hash bytea CHECK (candidate_hash IS NULL OR octet_length(candidate_hash) = 32),
    redacted_config text NOT NULL DEFAULT '' CHECK (octet_length(redacted_config) <= 262144),
    updated_at timestamptz NOT NULL
);

CREATE TABLE config_plans (
    id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE RESTRICT,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    operation_id uuid NOT NULL UNIQUE REFERENCES operations(id) ON DELETE RESTRICT,
    template_name text NOT NULL CHECK (length(template_name) BETWEEN 1 AND 128),
    expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
    candidate_hash bytea NOT NULL CHECK (octet_length(candidate_hash) = 32),
    candidate_redacted text NOT NULL CHECK (octet_length(candidate_redacted) BETWEEN 1 AND 262144),
    warnings jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(warnings) = 'array'),
    expires_at timestamptz NOT NULL,
    created_by uuid REFERENCES identities(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    FOREIGN KEY (workspace_id, node_id) REFERENCES nodes(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX config_plans_node_created_idx ON config_plans(node_id, created_at DESC, id DESC);

COMMENT ON TABLE config_plans IS 'Immutable, redacted, side-effect-free configuration plan snapshots.';
COMMENT ON COLUMN config_plans.candidate_redacted IS 'Rendered candidate with every SecretRef represented by a non-secret placeholder.';
