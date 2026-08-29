-- Durable fleet agent upgrade rollouts. The Control Plane owns the canary and
-- batch advancement; the browser never loops over per-node upgrade calls.
CREATE TABLE agent_rollouts (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    target_version text NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (
        state IN ('queued', 'running', 'paused', 'succeeded', 'failed', 'cancelled')
    ),
    batch_size integer NOT NULL CHECK (batch_size BETWEEN 1 AND 20),
    stop_on_failure boolean NOT NULL DEFAULT true,
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 512),
    approval_id uuid NOT NULL REFERENCES approval_requests(id) ON DELETE RESTRICT,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_by uuid NOT NULL,
    actor_session_id uuid NOT NULL,
    current_batch integer NOT NULL DEFAULT 0 CHECK (current_batch >= 0),
    pause_code text NOT NULL DEFAULT '' CHECK (length(pause_code) <= 128),
    exclusions jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (
        jsonb_typeof(exclusions) = 'array' AND jsonb_array_length(exclusions) <= 500
    ),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (workspace_id, idempotency_key)
);

COMMENT ON TABLE agent_rollouts IS
    'Durable canary and rolling agent upgrade orchestration; state survives Controller restart and browser closure.';

CREATE INDEX agent_rollouts_workspace_created_idx
    ON agent_rollouts (workspace_id, created_at DESC);

CREATE INDEX agent_rollouts_active_idx
    ON agent_rollouts (created_at)
    WHERE state IN ('queued', 'running');

-- One row per selected node in stable sorted order. Batch 0 is the mandatory
-- single-node canary; later batches hold at most batch_size ordinals.
CREATE TABLE agent_rollout_nodes (
    rollout_id uuid NOT NULL REFERENCES agent_rollouts(id) ON DELETE CASCADE,
    node_id uuid NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    batch integer NOT NULL CHECK (batch >= 0),
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending', 'running', 'succeeded', 'failed', 'rolled_back', 'unknown', 'skipped')
    ),
    operation_id uuid,
    from_version text NOT NULL DEFAULT '',
    failure_code text NOT NULL DEFAULT '' CHECK (length(failure_code) <= 128),
    dispatch_node_version bigint,
    dispatch_attempt integer NOT NULL DEFAULT 0 CHECK (dispatch_attempt >= 0),
    dispatch_lease_until timestamptz,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (rollout_id, node_id),
    UNIQUE (rollout_id, ordinal)
);

COMMENT ON TABLE agent_rollout_nodes IS
    'Per-node rollout projection; each upgrade reuses the reconciled single-node operation via operation_id.';

CREATE INDEX agent_rollout_nodes_active_idx
    ON agent_rollout_nodes (rollout_id, batch)
    WHERE state IN ('pending', 'running');

CREATE INDEX agent_rollout_nodes_operation_idx
    ON agent_rollout_nodes (operation_id)
    WHERE operation_id IS NOT NULL;
