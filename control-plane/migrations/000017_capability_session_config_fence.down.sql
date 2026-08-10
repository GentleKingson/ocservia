DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM agent_command_results
        WHERE semantic_payload_hash_version = 2
    ) THEN
        RAISE EXCEPTION 'cannot roll back session authority while semantic hash v2 results exist';
    END IF;
END;
$$;

ALTER TABLE agent_command_results
    DROP CONSTRAINT agent_command_results_semantic_payload_hash_version_supported;
ALTER TABLE agent_command_results
    ADD CONSTRAINT agent_command_results_semantic_payload_hash_version_supported CHECK (
        semantic_payload_hash_version IN (0, 1)
    );
COMMENT ON COLUMN agent_command_results.semantic_payload_hash_version IS
    'Algorithm version that produced payload_sha256 (0 = legacy, 1 = v1 canonical semantic hash).';

ALTER TABLE config_apply_operations
    DROP CONSTRAINT config_apply_operations_desired_revision_check;
ALTER TABLE config_apply_operations
    ADD CONSTRAINT config_apply_operations_desired_revision_check CHECK (
        expected_revision >= 0 AND desired_revision = expected_revision + 1
    );

ALTER TABLE node_config_state DROP COLUMN desired_revision;
ALTER TABLE nodes DROP COLUMN authorization_revision;
