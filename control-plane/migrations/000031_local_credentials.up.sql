CREATE TABLE local_credentials (
    identity_id uuid PRIMARY KEY REFERENCES identities(id) ON DELETE CASCADE,
    username text NOT NULL UNIQUE CHECK (
        octet_length(username) BETWEEN 1 AND 128 AND
        username COLLATE "C" ~ '^[a-z0-9][a-z0-9._-]*$'
    ),
    password_hash text NOT NULL CHECK (octet_length(password_hash) BETWEEN 1 AND 512),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    password_changed_at timestamptz NOT NULL
);

-- The new credentials are not read by older Controllers. Identity and session
-- schemas and their authorization meaning remain unchanged.
UPDATE controller_schema_compatibility
SET minimum_compatible_controller_schema = 29
WHERE singleton AND "current_schema" = 31;
