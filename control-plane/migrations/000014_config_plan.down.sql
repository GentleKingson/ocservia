DROP TABLE config_plans;
DROP TABLE node_config_state;
ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop', 'synthetic_echo', 'session_disconnect', 'session_terminate', 'ip_ban_remove', 'service_reload', 'user_create', 'user_disable', 'user_enable', 'user_password_rotate', 'group_apply')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Only typed command payloads are dispatchable; raw shell, occtl, and systemctl operations are forbidden.';
