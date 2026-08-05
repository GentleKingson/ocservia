ALTER TABLE agent_command_results
    DROP CONSTRAINT IF EXISTS agent_command_results_semantic_payload_hash_version_supported,
    DROP CONSTRAINT IF EXISTS agent_command_results_semantic_payload_hash_version_check,
    ADD CONSTRAINT agent_command_results_semantic_payload_hash_version_check
        CHECK (semantic_payload_hash_version >= 0);
