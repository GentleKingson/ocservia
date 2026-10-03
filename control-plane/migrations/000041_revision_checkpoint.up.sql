CREATE TABLE schema_revisions (
    epoch bigint NOT NULL CHECK (epoch > 0),
    revision bigint NOT NULL CHECK (revision >= 0),
    checksum bytea NOT NULL CHECK (octet_length(checksum) = 32),
    state text NOT NULL CHECK (state IN ('running', 'verified')),
    step integer NOT NULL CHECK (step >= 0),
    started_at timestamptz NOT NULL DEFAULT now(),
    verified_at timestamptz,
    PRIMARY KEY (epoch, revision),
    CHECK ((state = 'verified') = (verified_at IS NOT NULL))
);
