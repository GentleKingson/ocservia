ALTER TABLE commands
    DROP CONSTRAINT commands_payload_type_check,
    ADD CONSTRAINT commands_payload_type_check CHECK (
        payload_type IN ('synthetic_noop', 'synthetic_echo')
    );

DROP TABLE node_ip_bans;
