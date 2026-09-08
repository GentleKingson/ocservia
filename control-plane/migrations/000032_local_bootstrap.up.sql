CREATE TABLE local_auth_bootstrap (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    identity_id uuid NOT NULL REFERENCES identities(id) ON DELETE RESTRICT,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL
);

COMMENT ON TABLE local_auth_bootstrap IS 'One-shot Local initialization marker and fixed RBAC management workspace. Never cleared by password changes or disable.';
