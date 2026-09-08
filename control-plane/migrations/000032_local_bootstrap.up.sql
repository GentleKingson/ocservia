CREATE TABLE local_auth_bootstrap (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    identity_id uuid NOT NULL REFERENCES identities(id) ON DELETE RESTRICT,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL
);

COMMENT ON TABLE local_auth_bootstrap IS 'One-shot Local initialization marker and fixed RBAC management workspace. Never cleared by password changes or disable.';

-- Older Controllers do not read this additive marker. Existing identity,
-- credential, session and role-binding schemas remain unchanged.
UPDATE controller_schema_compatibility
SET minimum_compatible_controller_schema = 29
WHERE singleton AND "current_schema" = 32;
