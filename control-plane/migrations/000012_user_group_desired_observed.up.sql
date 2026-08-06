ALTER TABLE commands
    DROP CONSTRAINT commands_payload_type_check,
    ADD CONSTRAINT commands_payload_type_check CHECK (
        payload_type IN (
            'synthetic_noop', 'synthetic_echo', 'session_disconnect',
            'session_terminate', 'ip_ban_remove', 'service_reload',
            'user_create', 'user_disable', 'user_enable', 'user_password_rotate', 'group_apply'
        )
    ),
    DROP CONSTRAINT commands_expected_version_check,
    ADD CONSTRAINT commands_expected_version_check CHECK (expected_version >= 0),
    ADD COLUMN resource_type text,
    ADD COLUMN resource_key text,
    ADD CONSTRAINT commands_resource_identity CHECK (
        (resource_type IS NULL) = (resource_key IS NULL)
        AND (resource_type IS NULL OR resource_type IN ('user', 'group'))
    );

CREATE INDEX commands_pending_resource_idx
    ON commands (node_id, resource_type, resource_key, created_at)
    WHERE state = 'queued' AND resource_type IS NOT NULL;

CREATE TABLE desired_users (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    username text NOT NULL,
    enabled boolean NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, username),
    CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$')
);

CREATE TABLE desired_groups (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    group_name text NOT NULL,
    members text[] NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, group_name),
    CHECK (group_name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    CHECK (cardinality(members) <= 4096)
);

CREATE TABLE observed_users (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    username text NOT NULL,
    enabled boolean NOT NULL,
    revision bigint NOT NULL CHECK (revision >= 0),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, username),
    CHECK (username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$')
);

CREATE TABLE observed_groups (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    group_name text NOT NULL,
    members text[] NOT NULL,
    revision bigint NOT NULL CHECK (revision >= 0),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, group_name),
    CHECK (group_name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    CHECK (cardinality(members) <= 4096)
);

COMMENT ON TABLE desired_users IS 'Node-scoped desired user state. Password material is never stored here.';
COMMENT ON TABLE observed_users IS 'Agent-reported user state without password hashes or password material.';
COMMENT ON COLUMN commands.envelope IS 'Serialized typed command. Password commands may contain only client-sealed ciphertext, never plaintext.';
