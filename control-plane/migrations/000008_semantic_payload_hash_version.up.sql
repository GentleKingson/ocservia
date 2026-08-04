ALTER TABLE agent_command_results
    ADD COLUMN semantic_payload_hash_version smallint NOT NULL DEFAULT 0
    CHECK (semantic_payload_hash_version >= 0);

COMMENT ON COLUMN agent_command_results.semantic_payload_hash_version IS
    'Algorithm version that produced payload_sha256 (0 = legacy, 1 = v1 canonical semantic hash).';
