CREATE TABLE desired_user_policies (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    username text NOT NULL,
    quota_period text NOT NULL CHECK (quota_period IN ('none', 'monthly', 'lifetime')),
    quota_direction text NOT NULL CHECK (quota_direction IN ('rx', 'tx', 'rxtx')),
    quota_bytes bigint NOT NULL CHECK (quota_bytes BETWEEN 0 AND 9007199254740991),
    expires_at timestamptz,
    version bigint NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, username),
    FOREIGN KEY (node_id, username) REFERENCES desired_users(node_id, username) ON DELETE CASCADE,
    CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    CHECK ((quota_period = 'none') = (quota_bytes = 0))
);

CREATE TABLE user_policy_mutations (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid NOT NULL,
    username text NOT NULL,
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    created_at timestamptz NOT NULL,
    UNIQUE (workspace_id, idempotency_key),
    FOREIGN KEY (node_id, username) REFERENCES desired_user_policies(node_id, username) ON DELETE RESTRICT
);

CREATE TABLE observed_user_usage (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    username text NOT NULL,
    period text NOT NULL CHECK (period IN ('monthly', 'lifetime')),
    period_start timestamptz NOT NULL,
    rx_bytes bigint NOT NULL DEFAULT 0 CHECK (rx_bytes >= 0),
    tx_bytes bigint NOT NULL DEFAULT 0 CHECK (tx_bytes >= 0),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, username, period, period_start),
    CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$')
);

CREATE TABLE user_usage_cursors (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    session_id text NOT NULL CHECK (length(session_id) BETWEEN 1 AND 256),
    connected_at timestamptz NOT NULL,
    username text NOT NULL CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    rx_bytes bigint NOT NULL CHECK (rx_bytes >= 0),
    tx_bytes bigint NOT NULL CHECK (tx_bytes >= 0),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, session_id, connected_at)
);

CREATE TABLE scheduler_leases (
    lease_name text PRIMARY KEY CHECK (length(lease_name) BETWEEN 1 AND 128),
    owner_id uuid NOT NULL,
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE user_policy_enforcements (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    username text NOT NULL,
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    cause text NOT NULL CHECK (cause IN ('quota', 'expiry', 'quota_reset')),
    period_start timestamptz NOT NULL,
    source_user_version bigint NOT NULL CHECK (source_user_version > 0),
    operation_id uuid REFERENCES operations(id) ON DELETE RESTRICT,
    resulting_user_version bigint CHECK (resulting_user_version > 0),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, username, policy_version, cause, period_start),
    CHECK ((operation_id IS NULL) = (resulting_user_version IS NULL))
);

ALTER TABLE approval_requests
    ADD COLUMN request_hash bytea CHECK (request_hash IS NULL OR octet_length(request_hash) = 32),
    ADD COLUMN request_summary jsonb CHECK (request_summary IS NULL OR jsonb_typeof(request_summary) = 'array'),
    ADD CONSTRAINT approval_request_content_pair CHECK ((request_hash IS NULL) = (request_summary IS NULL));

CREATE TABLE batch_operations (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    state text NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'partial_failed', 'failed')),
    actor_identity_id uuid REFERENCES identities(id) ON DELETE RESTRICT,
    actor_session_id uuid REFERENCES auth_sessions(id) ON DELETE RESTRICT,
    approval_id uuid REFERENCES approval_requests(id) ON DELETE RESTRICT,
    actor_id text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 256),
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 512),
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    traceparent text NOT NULL CHECK (traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (workspace_id, idempotency_key)
);

CREATE TABLE batch_operation_items (
    batch_id uuid NOT NULL REFERENCES batch_operations(id) ON DELETE CASCADE,
    item_index integer NOT NULL CHECK (item_index >= 0),
    node_id uuid NOT NULL,
    username text NOT NULL CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    action text NOT NULL CHECK (action IN ('disable', 'enable')),
    expected_version bigint NOT NULL CHECK (expected_version > 0),
    state text NOT NULL CHECK (state IN ('queued', 'submitting', 'submitted', 'succeeded', 'failed', 'unknown', 'offline_pending', 'forbidden')),
    child_operation_id uuid REFERENCES operations(id) ON DELETE RESTRICT,
    error_type text,
    lease_owner uuid,
    lease_until timestamptz,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (batch_id, item_index),
    FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT,
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL))
);
CREATE INDEX batch_operation_items_claim_idx
    ON batch_operation_items (updated_at, batch_id, item_index)
    WHERE state IN ('queued', 'submitting');
CREATE INDEX batch_operations_active_idx
    ON batch_operations (updated_at, id)
    WHERE state IN ('queued', 'running', 'partial_failed');

CREATE INDEX operations_active_command_limit_idx
    ON operations (state, id)
    WHERE state IN ('dispatched', 'accepted', 'running', 'unknown');

CREATE INDEX operations_queued_backlog_idx
    ON operations (workspace_id, node_id, id)
    WHERE state IN ('queued', 'offline_pending');

CREATE TABLE upstream_sync_records (
    id uuid PRIMARY KEY,
    repository text NOT NULL,
    old_ref text NOT NULL,
    old_commit text NOT NULL CHECK (old_commit ~ '^[0-9a-f]{40}$'),
    new_ref text NOT NULL,
    new_commit text NOT NULL CHECK (new_commit ~ '^[0-9a-f]{40}$'),
    classification jsonb NOT NULL CHECK (jsonb_typeof(classification) = 'object'),
    rollback_ref text NOT NULL,
    synced_at timestamptz NOT NULL,
    UNIQUE (repository, old_commit, new_commit)
);

INSERT INTO upstream_sync_records(id,repository,old_ref,old_commit,new_ref,new_commit,classification,rollback_ref,synced_at)
VALUES(
    '019fdc5b-b939-72a1-ae67-8efd197e5688',
    'mmtaee/ocserv-dashboard',
    'v4.9',
    'b8f59026c4d879f40c1da43dc00d97e34f9790bc',
    'master',
    '4d25478580d899b77460bdf0cf0a590cfdd26030',
    '{"A":[],"B":["web/src/components/auth/SetupForm.vue"],"C":["quota and expiry semantics mapped to node-scoped desired policy and scheduler"],"D":["Docker/native occtl execution","local cron journal","direct password/config files","permanent deletion"]}'::jsonb,
    'publication: revert PR15 independently; implementation: stop I14 scheduler/API, reconcile commands, revert PR14, then apply migration 000013 down only when policy and batch data need not be retained',
    '2026-08-07T16:41:52Z'
);

COMMENT ON COLUMN desired_user_policies.quota_bytes IS 'Integer bytes. Monthly periods reset at 00:00:00 UTC on the first day of each calendar month.';
COMMENT ON TABLE batch_operation_items IS 'Each item is authorized independently and owns a distinct child operation and command.';
COMMENT ON TABLE upstream_sync_records IS 'Pinned, auditable upstream comparison metadata; local execution and privileged deployment code are never imported.';
