ALTER TABLE schema_migrations ADD COLUMN snapshot_covered boolean NOT NULL DEFAULT false;

-- A row records coverage by one atomic schema snapshot, not execution of the
-- covered migrations. Legacy databases leave this table empty.
CREATE TABLE schema_snapshot_origin (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    covered_version bigint NOT NULL CHECK (covered_version > 0),
    history_sha256 bytea NOT NULL CHECK (octet_length(history_sha256) = 32),
    schema_sha256 bytea NOT NULL CHECK (octet_length(schema_sha256) = 32),
    receipt_sha256 bytea NOT NULL CHECK (octet_length(receipt_sha256) = 32),
    initialized_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE schema_snapshot_origin IS 'Atomic snapshot coverage provenance; covered schema_migrations rows are coverage, not individually executed migrations.';
