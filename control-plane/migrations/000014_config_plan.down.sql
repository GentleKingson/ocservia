DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM commands
        WHERE payload_type = 'config_plan'
          AND state IN ('queued', 'dispatched', 'accepted', 'running', 'unknown')
    ) THEN
        RAISE EXCEPTION 'cannot remove config planning while config_plan commands are nonterminal; stop planning and reconcile or expire them first';
    END IF;
END
$$;

DROP TABLE config_plans;
DROP TABLE node_config_state;
ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop', 'synthetic_echo', 'session_disconnect', 'session_terminate', 'ip_ban_remove', 'service_reload', 'user_create', 'user_disable', 'user_enable', 'user_password_rotate', 'group_apply', 'config_plan')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Typed config_plan history remains admissible after rollback; older binaries cannot create it. Raw shell, occtl, and systemctl operations are forbidden.';
