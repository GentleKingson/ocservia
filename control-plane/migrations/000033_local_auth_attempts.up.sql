CREATE TABLE local_auth_attempts (
    username text PRIMARY KEY CHECK (
        octet_length(username) BETWEEN 1 AND 128 AND
        username COLLATE "C" ~ '^[a-z0-9][a-z0-9._-]*$'
    ),
    failures integer NOT NULL DEFAULT 0 CHECK (failures BETWEEN 0 AND 14),
    window_until timestamptz NOT NULL,
    blocked_until timestamptz NOT NULL DEFAULT '-infinity',
    lease_id uuid,
    lease_until timestamptz NOT NULL DEFAULT '-infinity',
    expires_at timestamptz NOT NULL
);
CREATE INDEX local_auth_attempts_expiry ON local_auth_attempts(expires_at);

-- Keep the runner's minimum compatible schema at 33: older Controllers do
-- not enforce account admission and must not serve Local login in this epoch.
