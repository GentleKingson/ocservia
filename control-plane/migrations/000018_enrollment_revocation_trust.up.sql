CREATE TABLE node_trust_convergence (
    node_id uuid PRIMARY KEY REFERENCES nodes(id) ON DELETE RESTRICT,
    endpoint_id bytea NOT NULL CHECK (octet_length(endpoint_id) = 32),
    desired_state text NOT NULL CHECK (desired_state IN ('active', 'revoked')),
    revision bigint NOT NULL CHECK (revision > 0),
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1024),
    update_applied boolean NOT NULL DEFAULT false,
    close_required boolean NOT NULL,
    close_applied boolean NOT NULL DEFAULT false,
    available_at timestamptz NOT NULL,
    locked_by uuid,
    locked_until timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK ((locked_by IS NULL) = (locked_until IS NULL)),
    CHECK (NOT close_applied OR close_required),
    CHECK (desired_state = 'revoked' OR NOT close_required)
);

CREATE INDEX node_trust_convergence_pending_idx
    ON node_trust_convergence (available_at, node_id)
    WHERE NOT update_applied OR (close_required AND NOT close_applied);

INSERT INTO node_trust_convergence
    (node_id, endpoint_id, desired_state, revision, reason, close_required,
     available_at, created_at, updated_at)
SELECT n.id, k.endpoint_id,
       CASE WHEN n.status = 'revoked' THEN 'revoked' ELSE 'active' END,
       n.authorization_revision,
       'migration trust convergence',
       n.status = 'revoked',
       now(), now(), now()
FROM nodes n
JOIN node_endpoint_keys k ON k.node_id = n.id
WHERE (n.status IN ('active', 'offline') AND k.state = 'active')
   OR (n.status = 'revoked' AND k.state = 'revoked');

COMMENT ON TABLE node_trust_convergence IS
    'Durable Controller-to-transport trust convergence; node trust changes and retry work commit atomically.';
