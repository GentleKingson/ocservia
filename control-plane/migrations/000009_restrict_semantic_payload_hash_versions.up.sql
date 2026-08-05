ALTER TABLE agent_command_results
    DROP CONSTRAINT IF EXISTS agent_command_results_semantic_payload_hash_version_check,
    ADD CONSTRAINT agent_command_results_semantic_payload_hash_version_supported
        CHECK (semantic_payload_hash_version IN (0, 1));
