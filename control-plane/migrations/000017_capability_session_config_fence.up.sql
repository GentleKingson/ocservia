ALTER TABLE nodes
    ADD COLUMN authorization_revision bigint NOT NULL DEFAULT 1 CHECK (authorization_revision > 0);

UPDATE nodes
SET authorization_revision = GREATEST(version, 1)
WHERE authorization_revision <> GREATEST(version, 1);

ALTER TABLE agent_command_results
    DROP CONSTRAINT agent_command_results_semantic_payload_hash_version_supported;
ALTER TABLE agent_command_results
    ADD CONSTRAINT agent_command_results_semantic_payload_hash_version_supported CHECK (
        semantic_payload_hash_version IN (0, 1, 2)
    );
COMMENT ON COLUMN agent_command_results.semantic_payload_hash_version IS
    'Algorithm version that produced payload_sha256 (0 = legacy, 1 = frozen v1, 2 = session-authority v2).';

ALTER TABLE node_config_state
    ADD COLUMN desired_revision bigint NOT NULL DEFAULT 0 CHECK (desired_revision >= revision);

UPDATE node_config_state SET desired_revision = revision;

ALTER TABLE config_apply_operations
    DROP CONSTRAINT config_apply_operations_desired_revision_check;

ALTER TABLE config_apply_operations
    ADD CONSTRAINT config_apply_operations_desired_revision_check CHECK (
        expected_revision >= 0 AND desired_revision > expected_revision
    );

COMMENT ON COLUMN nodes.authorization_revision IS
    'Monotonic authority epoch changed only by trust or capability authorization transitions.';
COMMENT ON COLUMN node_config_state.desired_revision IS
    'Highest Controller-issued configuration effect revision, independent of the last applied revision.';
