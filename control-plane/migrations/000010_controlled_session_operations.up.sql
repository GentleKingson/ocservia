ALTER TABLE commands
    DROP CONSTRAINT commands_payload_type_check,
    ADD CONSTRAINT commands_payload_type_check CHECK (
        payload_type IN (
            'synthetic_noop',
            'synthetic_echo',
            'session_disconnect',
            'session_terminate',
            'ip_ban_remove',
            'service_reload'
        )
    );

COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Only typed command payloads are dispatchable; raw shell, occtl, and systemctl operations are forbidden.';

CREATE TABLE node_ip_bans (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ip inet NOT NULL,
    seconds_remaining bigint CHECK (seconds_remaining IS NULL OR seconds_remaining >= 0),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, ip)
);

COMMENT ON TABLE node_ip_bans IS
    'Current typed Ocserv ban observations; addresses must not be used as metric labels.';
