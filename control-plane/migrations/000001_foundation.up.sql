CREATE TABLE workspaces (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    slug text NOT NULL UNIQUE,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz
);

CREATE TABLE nodes (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    name text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'approved', 'revoked', 'offline')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (workspace_id, name),
    UNIQUE (workspace_id, id)
);

CREATE TABLE operations (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid,
    state text NOT NULL CHECK (state IN ('draft', 'queued', 'dispatched', 'accepted', 'running', 'succeeded', 'failed', 'unknown', 'expired', 'rolled_back', 'offline_pending', 'drifted', 'superseded')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    request_id text NOT NULL,
    trace_id text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (workspace_id, node_id) REFERENCES nodes(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX operations_workspace_created_idx ON operations (workspace_id, created_at DESC, id DESC);

CREATE TABLE audit_events (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    occurred_at timestamptz NOT NULL,
    actor_type text NOT NULL,
    actor_id text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid,
    request_id text NOT NULL,
    trace_id text,
    result text NOT NULL CHECK (result IN ('intent', 'succeeded', 'failed')),
    reason text,
    previous_event_hash bytea,
    event_hash bytea NOT NULL
);
CREATE INDEX audit_events_workspace_time_idx ON audit_events (workspace_id, occurred_at DESC, id DESC);

COMMENT ON TABLE audit_events IS 'Append-only security audit records; secrets and full request bodies are forbidden.';

CREATE FUNCTION reject_audit_event_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;

CREATE TRIGGER audit_events_append_only
BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_events
FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_event_mutation();

REVOKE ALL ON FUNCTION reject_audit_event_mutation() FROM PUBLIC;
