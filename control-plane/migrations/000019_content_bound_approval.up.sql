ALTER TABLE approval_requests
    DROP CONSTRAINT approval_requests_request_summary_check,
    ADD CONSTRAINT approval_requests_request_summary_check CHECK (
        request_summary IS NULL OR jsonb_typeof(request_summary) IN ('array','object')
    ),
    ADD COLUMN authority_snapshot_at timestamptz;

UPDATE approval_requests SET authority_snapshot_at=created_at WHERE authority_snapshot_at IS NULL;
ALTER TABLE approval_requests ALTER COLUMN authority_snapshot_at SET NOT NULL;
ALTER TABLE approval_requests ALTER COLUMN authority_snapshot_at SET DEFAULT now();

CREATE TABLE approval_authority_resources (
    approval_id uuid NOT NULL REFERENCES approval_requests(id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    resource_type text NOT NULL CHECK (resource_type IN ('workspace','node','resource','secret_ref','certificate','config_plan','batch_operation','role_binding')),
    resource_id uuid NOT NULL,
    PRIMARY KEY (approval_id,resource_type,resource_id)
);

ALTER TABLE role_bindings
    ADD COLUMN approval_id uuid REFERENCES approval_requests(id) ON DELETE RESTRICT;
ALTER TABLE role_bindings DROP CONSTRAINT role_bindings_resource_type_check;
ALTER TABLE role_bindings ADD CONSTRAINT role_bindings_resource_type_check CHECK (resource_type IN ('workspace','node','resource','secret_ref','certificate','config_plan','batch_operation','role_binding'));

ALTER TABLE artifact_operations
    ADD COLUMN approval_id uuid REFERENCES approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE certificates ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0);

CREATE TABLE approval_batch_items (
    approval_id uuid NOT NULL REFERENCES approval_requests(id) ON DELETE CASCADE,
    item_index integer NOT NULL CHECK (item_index >= 0),
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    username text NOT NULL CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    action text NOT NULL CHECK (action IN ('disable','enable')),
    expected_version bigint NOT NULL CHECK (expected_version > 0),
    PRIMARY KEY (approval_id,item_index)
);

GRANT SELECT, INSERT ON approval_authority_resources TO ocservia_app;
GRANT SELECT, INSERT ON approval_batch_items TO ocservia_app;
