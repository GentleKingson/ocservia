ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop', 'synthetic_echo', 'session_disconnect', 'session_terminate', 'ip_ban_remove', 'service_reload', 'user_create', 'user_disable', 'user_enable', 'user_password_rotate', 'group_apply', 'config_plan', 'config_apply')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Only typed command payloads are dispatchable; raw shell, occtl, and systemctl operations are forbidden.';

ALTER TABLE approval_requests DROP CONSTRAINT approval_requests_request_summary_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_request_summary_check CHECK (
    request_summary IS NULL OR jsonb_typeof(request_summary) IN ('array', 'object')
);
COMMENT ON CONSTRAINT approval_requests_request_summary_check ON approval_requests IS
    'Batch approvals use arrays; configuration approvals use a typed object summary.';

ALTER TABLE node_config_state
    ADD COLUMN automation_locked boolean NOT NULL DEFAULT false,
    ADD COLUMN automation_lock_reason text CHECK (automation_lock_reason IS NULL OR length(automation_lock_reason) BETWEEN 1 AND 128),
    ADD COLUMN last_apply_operation_id uuid REFERENCES operations(id) ON DELETE RESTRICT;

CREATE TABLE config_apply_operations (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE RESTRICT,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    plan_id uuid NOT NULL REFERENCES config_plans(id) ON DELETE RESTRICT,
    approval_id uuid NOT NULL REFERENCES approval_requests(id) ON DELETE RESTRICT,
    expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
    desired_revision bigint NOT NULL CHECK (desired_revision = expected_revision + 1),
    candidate_hash bytea NOT NULL CHECK (octet_length(candidate_hash) = 32),
    previous_hash bytea NOT NULL CHECK (octet_length(previous_hash) = 32),
    state text NOT NULL CHECK (state IN ('queued','dispatched','accepted','running','succeeded','failed','rolled_back','failed_critical','unknown','expired')),
    failure_code text CHECK (failure_code IS NULL OR length(failure_code) BETWEEN 1 AND 128),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE(plan_id),
    FOREIGN KEY (workspace_id, node_id) REFERENCES nodes(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX config_apply_operations_node_created_idx ON config_apply_operations(node_id, created_at DESC, operation_id DESC);
CREATE UNIQUE INDEX config_apply_operations_one_active_node_idx ON config_apply_operations(node_id)
    WHERE state IN ('queued','dispatched','accepted','running','unknown');
COMMENT ON TABLE config_apply_operations IS 'Approved configuration transactions and secret-safe apply/rollback outcomes.';
