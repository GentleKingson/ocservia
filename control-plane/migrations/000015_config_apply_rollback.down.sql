DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM commands
        WHERE payload_type = 'config_apply'
          AND state IN ('queued','dispatched','accepted','running','unknown')
    ) THEN
        RAISE EXCEPTION 'cannot roll back config apply while nonterminal config_apply commands exist';
    END IF;
END;
$$;

DROP TABLE config_apply_operations;
ALTER TABLE node_config_state
    DROP COLUMN last_apply_operation_id,
    DROP COLUMN automation_lock_reason,
    DROP COLUMN automation_locked;

ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop', 'synthetic_echo', 'session_disconnect', 'session_terminate', 'ip_ban_remove', 'service_reload', 'user_create', 'user_disable', 'user_enable', 'user_password_rotate', 'group_apply', 'config_plan', 'config_apply')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Only typed command payloads are dispatchable; terminal config apply history remains admissible after rollback.';
