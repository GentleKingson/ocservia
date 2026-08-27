-- Single-node agent upgrade reconciliation projections and the durable
-- node-reported upgrade outcome evidence consumed by the reconciliation loop.
CREATE TABLE agent_upgrade_operations (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE RESTRICT,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid NOT NULL,
    target_version text NOT NULL,
    package_sha256 bytea NOT NULL CHECK (octet_length(package_sha256) = 32),
    architecture text NOT NULL CHECK (architecture IN ('amd64', 'arm64')),
    from_version text NOT NULL DEFAULT '',
    approval_id uuid,
    state text NOT NULL CHECK (
        state IN ('queued', 'accepted', 'running', 'succeeded', 'failed', 'rolled_back', 'unknown')
    ),
    scheduled_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

COMMENT ON TABLE agent_upgrade_operations IS
    'Per-operation projection for reconciled single-node agent upgrades; mirrors the operations lifecycle.';

CREATE INDEX agent_upgrade_operations_pending_idx
    ON agent_upgrade_operations (node_id)
    WHERE state IN ('queued', 'accepted', 'running', 'unknown');

CREATE INDEX agent_upgrade_operations_operation_idx
    ON agent_upgrade_operations (operation_id);

CREATE TABLE node_agent_upgrade_results (
    operation_id uuid PRIMARY KEY,
    node_id uuid NOT NULL,
    state text NOT NULL CHECK (state IN ('succeeded', 'failed', 'rolled_back')),
    target_version text NOT NULL,
    detail text NOT NULL DEFAULT '' CHECK (length(detail) <= 160),
    completed_at timestamptz NOT NULL,
    reported_at timestamptz NOT NULL
);

COMMENT ON TABLE node_agent_upgrade_results IS
    'Durable local upgrader outcomes reported read-only through agent telemetry; first report per operation wins.';

CREATE INDEX node_agent_upgrade_results_node_idx
    ON node_agent_upgrade_results (node_id);

ALTER TABLE node_observed_snapshots
    ADD COLUMN architecture text NOT NULL DEFAULT '';
