-- The database owns the Controller schema compatibility contract. The
-- migration runner resets the range to exact for each later migration; a
-- migration must explicitly lower the minimum only after compatibility review.
CREATE TABLE controller_schema_compatibility (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    "current_schema" bigint NOT NULL CHECK ("current_schema" > 0),
    minimum_compatible_controller_schema bigint NOT NULL CHECK (minimum_compatible_controller_schema > 0),
    CHECK (minimum_compatible_controller_schema <= "current_schema")
);

COMMENT ON TABLE controller_schema_compatibility IS
    'Authoritative Controller schema compatibility range; not a backup or PITR marker.';

INSERT INTO controller_schema_compatibility (
    singleton,
    "current_schema",
    minimum_compatible_controller_schema
) VALUES (true, 29, 29);
