CREATE TABLE identities (
    id uuid PRIMARY KEY,
    issuer text NOT NULL,
    subject text NOT NULL,
    email text,
    display_name text,
    disabled_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (issuer, subject)
);

CREATE TABLE auth_sessions (
    id uuid PRIMARY KEY,
    identity_id uuid NOT NULL REFERENCES identities(id) ON DELETE RESTRICT,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    break_glass boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    CHECK (expires_at > created_at)
);
CREATE INDEX auth_sessions_expiry_idx ON auth_sessions (expires_at) WHERE revoked_at IS NULL;

CREATE TABLE roles (
    name text PRIMARY KEY CHECK (name IN (
        'Viewer','Operator','UserManager','ConfigManager','Auditor',
        'SecurityAdmin','PlatformAdmin'
    ))
);
INSERT INTO roles (name) VALUES
    ('Viewer'),('Operator'),('UserManager'),('ConfigManager'),('Auditor'),
    ('SecurityAdmin'),('PlatformAdmin');

CREATE TABLE role_bindings (
    id uuid PRIMARY KEY,
    identity_id uuid NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    role_name text NOT NULL REFERENCES roles(name) ON DELETE RESTRICT,
    resource_type text NOT NULL DEFAULT 'workspace' CHECK (resource_type IN ('workspace','node','resource')),
    resource_id uuid,
    created_by uuid REFERENCES identities(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    CHECK ((resource_type = 'workspace') = (resource_id IS NULL)),
    UNIQUE NULLS NOT DISTINCT (identity_id, workspace_id, role_name, resource_type, resource_id)
);
CREATE INDEX role_bindings_authorization_idx
    ON role_bindings (identity_id, workspace_id, resource_type, resource_id, role_name);

CREATE TABLE approval_requests (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    requester_id uuid NOT NULL REFERENCES identities(id) ON DELETE RESTRICT,
    action text NOT NULL CHECK (length(action) BETWEEN 1 AND 128),
    resource_type text NOT NULL CHECK (length(resource_type) BETWEEN 1 AND 64),
    resource_id uuid NOT NULL,
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 512),
    status text NOT NULL CHECK (status IN ('pending','approved','rejected','expired','consumed')),
    approver_id uuid REFERENCES identities(id) ON DELETE RESTRICT,
    approval_reason text,
    expires_at timestamptz NOT NULL,
    approved_at timestamptz,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL,
    CHECK (requester_id IS DISTINCT FROM approver_id),
    CHECK ((status IN ('approved','consumed')) = (approver_id IS NOT NULL)),
    CHECK ((status = 'consumed') = (consumed_at IS NOT NULL)),
    CHECK (expires_at > created_at)
);
CREATE INDEX approval_requests_scope_idx
    ON approval_requests (workspace_id, resource_type, resource_id, action, status, expires_at);

ALTER TABLE audit_events
    ADD COLUMN source_session_id uuid,
    ADD COLUMN node_id uuid REFERENCES nodes(id) ON DELETE RESTRICT,
    ADD COLUMN command_id uuid,
    ADD COLUMN approval_id uuid REFERENCES approval_requests(id) ON DELETE RESTRICT,
    ADD COLUMN before_summary jsonb,
    ADD COLUMN after_summary jsonb,
    ADD COLUMN error_type text,
    ADD CONSTRAINT audit_previous_hash_size CHECK (previous_event_hash IS NULL OR octet_length(previous_event_hash) = 32),
    ADD CONSTRAINT audit_event_hash_size CHECK (octet_length(event_hash) = 32);

CREATE TABLE audit_checkpoints (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    through_event_id uuid NOT NULL REFERENCES audit_events(id) ON DELETE RESTRICT,
    through_event_hash bytea NOT NULL CHECK (octet_length(through_event_hash) = 32),
    signature bytea NOT NULL CHECK (octet_length(signature) = 32),
    created_at timestamptz NOT NULL,
    UNIQUE (workspace_id, through_event_id)
);

CREATE TABLE security_alerts (
    id uuid PRIMARY KEY,
    workspace_id uuid REFERENCES workspaces(id) ON DELETE RESTRICT,
    severity text NOT NULL CHECK (severity IN ('high','critical')),
    kind text NOT NULL CHECK (length(kind) BETWEEN 1 AND 128),
    source_session_id uuid,
    acknowledged_at timestamptz,
    created_at timestamptz NOT NULL
);

CREATE TABLE break_glass_uses (
    credential_fingerprint bytea PRIMARY KEY CHECK (octet_length(credential_fingerprint) = 32),
    identity_id uuid NOT NULL REFERENCES identities(id) ON DELETE RESTRICT,
    used_at timestamptz NOT NULL,
    source_session_id uuid NOT NULL,
    rotation_required boolean NOT NULL DEFAULT true
);

CREATE FUNCTION reject_audit_checkpoint_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_checkpoints are append-only';
END;
$$;

CREATE TRIGGER audit_checkpoints_append_only
BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_checkpoints
FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_checkpoint_mutation();

REVOKE ALL ON FUNCTION reject_audit_checkpoint_mutation() FROM PUBLIC;
