BEGIN;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM commands WHERE payload_type='agent_upgrade') THEN
    RAISE EXCEPTION 'cannot roll back the typed agent upgrade contract while agent upgrade command history exists';
  END IF;
END $$;

ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop','synthetic_echo','session_disconnect','session_terminate','ip_ban_remove','service_reload','user_create','user_disable','user_enable','user_password_rotate','group_apply','config_plan','config_apply','certificate_csr','certificate_p12','certificate_revoke')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Only typed command payloads are dispatchable; raw shell, file, occtl, and systemctl operations are forbidden.';

COMMIT;
