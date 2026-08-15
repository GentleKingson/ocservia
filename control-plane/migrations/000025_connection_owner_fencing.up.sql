CREATE TABLE IF NOT EXISTS connection_owner_fencing (
    node_id bytea PRIMARY KEY CHECK (octet_length(node_id) = 16),
    owner_instance_id uuid NOT NULL,
    owner_incarnation bigint NOT NULL CHECK (owner_incarnation >= 0),
    connection_id bytea NOT NULL CHECK (octet_length(connection_id) = 16),
    owner_epoch bigint NOT NULL CHECK (owner_epoch >= 1),
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Each node owns exactly one row, created lazily by the first Acquire with
-- owner_epoch 1. There is deliberately no seed row and no default epoch: the
-- per-node fencing epoch only ever grows through Acquire, so a schema
-- rollback or re-upgrade can neither reset nor reuse observed epochs (see
-- the down script). Re-running this migration over retained state is a
-- no-op because of IF NOT EXISTS.
