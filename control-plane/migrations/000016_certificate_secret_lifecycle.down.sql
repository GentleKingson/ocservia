DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM commands WHERE payload_type IN ('certificate_csr','certificate_p12','certificate_revoke') AND state IN ('queued','dispatched','accepted','running','unknown')) THEN
    RAISE EXCEPTION 'cannot roll back certificate lifecycle while certificate commands are nonterminal';
  END IF;
END $$;

DROP TABLE secret_provider_refs;
DROP TABLE artifact_operations;
DROP TABLE certificates;

ALTER TABLE security_alerts
    DROP COLUMN resource_id,
    DROP COLUMN resource_type,
    DROP COLUMN node_id;

ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop','synthetic_echo','session_disconnect','session_terminate','ip_ban_remove','service_reload','user_create','user_disable','user_enable','user_password_rotate','group_apply','config_plan','config_apply','certificate_csr','certificate_p12','certificate_revoke')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Typed certificate history remains admissible after rollback; older runtimes cannot create it.';
