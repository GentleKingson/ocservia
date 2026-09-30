ALTER TABLE node_bootstrap_tokens ADD COLUMN expected_endpoint_id bytea
    CHECK (expected_endpoint_id IS NULL OR octet_length(expected_endpoint_id) = 32);

-- Older Controllers do not enforce this authorization restriction.
UPDATE controller_schema_compatibility SET "current_schema"=37,minimum_compatible_controller_schema=37 WHERE singleton;
