DROP TABLE IF EXISTS observed_groups;
DROP TABLE IF EXISTS observed_users;
DROP TABLE IF EXISTS desired_groups;
DROP TABLE IF EXISTS desired_users;
DROP INDEX IF EXISTS commands_pending_resource_idx;
ALTER TABLE commands
    DROP CONSTRAINT commands_resource_identity,
    DROP COLUMN resource_key,
    DROP COLUMN resource_type,
    DROP CONSTRAINT commands_expected_version_check,
    ADD CONSTRAINT commands_expected_version_check CHECK (expected_version > 0),
    DROP CONSTRAINT commands_payload_type_check,
    ADD CONSTRAINT commands_payload_type_check CHECK (
        payload_type IN ('synthetic_noop', 'synthetic_echo', 'session_disconnect', 'session_terminate', 'ip_ban_remove', 'service_reload')
    );
