ALTER TABLE commands DROP CONSTRAINT commands_payload_type_check;
ALTER TABLE commands ADD CONSTRAINT commands_payload_type_check CHECK (
    payload_type IN ('synthetic_noop','synthetic_echo','session_disconnect','session_terminate','ip_ban_remove','service_reload','user_create','user_disable','user_enable','user_password_rotate','group_apply','config_plan','config_apply','certificate_csr','certificate_p12','certificate_revoke')
);
COMMENT ON CONSTRAINT commands_payload_type_check ON commands IS
    'Only typed command payloads are dispatchable; raw shell, file, occtl, and systemctl operations are forbidden.';

CREATE TABLE certificates (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    operation_id uuid NOT NULL UNIQUE REFERENCES operations(id) ON DELETE RESTRICT,
    common_name text NOT NULL CHECK (length(common_name) BETWEEN 1 AND 253),
    dns_names jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(dns_names)='array'),
    key_bits integer NOT NULL CHECK (key_bits IN (2048,3072,4096)),
    state text NOT NULL CHECK (state IN ('csr_pending','csr_ready','signing','signer_unavailable','issued','expiring','expired','revoking','revocation_unknown','revoked','failed','unknown')),
    csr_der bytea CHECK (csr_der IS NULL OR octet_length(csr_der) BETWEEN 64 AND 65536),
    public_key_sha256 bytea CHECK (public_key_sha256 IS NULL OR octet_length(public_key_sha256)=32),
    certificate_chain_pem bytea CHECK (certificate_chain_pem IS NULL OR octet_length(certificate_chain_pem) BETWEEN 64 AND 262144),
    serial_number text CHECK (serial_number IS NULL OR length(serial_number) BETWEEN 1 AND 128),
    not_before timestamptz,
    not_after timestamptz,
    revoked_at timestamptz,
    revocation_reason text CHECK (revocation_reason IS NULL OR length(revocation_reason) BETWEEN 1 AND 128),
    issue_approval_id uuid REFERENCES approval_requests(id) ON DELETE RESTRICT,
    issue_request_hash bytea CHECK (issue_request_hash IS NULL OR octet_length(issue_request_hash)=32),
    issue_actor_identity_id uuid REFERENCES identities(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (workspace_id,node_id) REFERENCES nodes(workspace_id,id) ON DELETE RESTRICT
);
CREATE INDEX certificates_node_expiry_idx ON certificates(node_id,not_after) WHERE state IN ('issued','expiring');

CREATE TABLE artifact_operations (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    certificate_id uuid NOT NULL REFERENCES certificates(id) ON DELETE RESTRICT,
    operation_id uuid NOT NULL UNIQUE REFERENCES operations(id) ON DELETE RESTRICT,
    purpose text NOT NULL CHECK (purpose='certificate_p12'),
    state text NOT NULL CHECK (state IN ('pending','ready','leased','consumed','expired','failed')),
    content_sha256 bytea CHECK (content_sha256 IS NULL OR octet_length(content_sha256)=32),
    content_size bigint CHECK (content_size IS NULL OR content_size BETWEEN 1 AND 67108864),
    token_sha256 bytea NOT NULL CHECK (octet_length(token_sha256)=32),
	request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
    expires_at timestamptz NOT NULL,
    lease_until timestamptz,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
	CHECK ((state='ready' AND content_sha256 IS NOT NULL AND content_size IS NOT NULL) OR state<>'ready')
);
CREATE UNIQUE INDEX artifact_operations_one_live_certificate_idx ON artifact_operations(certificate_id) WHERE state IN ('pending','ready','leased');
CREATE INDEX artifact_operations_expiry_idx ON artifact_operations(expires_at) WHERE state IN ('pending','ready','leased');

CREATE TABLE secret_provider_refs (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    provider text NOT NULL CHECK (provider ~ '^[A-Za-z0-9._-]{1,64}$'),
    key_path text NOT NULL CHECK (length(key_path) BETWEEN 1 AND 512 AND key_path !~ '(^|/)\.\.(/|$)'),
    version text NOT NULL CHECK (length(version) BETWEEN 1 AND 128),
    state text NOT NULL CHECK (state IN ('active','rotating','disabled','unavailable')),
    rotated_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE(workspace_id,provider,key_path)
);
COMMENT ON TABLE secret_provider_refs IS 'Opaque external secret references only; secret values are never stored in PostgreSQL.';

ALTER TABLE security_alerts
    ADD COLUMN node_id uuid REFERENCES nodes(id) ON DELETE RESTRICT,
    ADD COLUMN resource_type text CHECK (resource_type IS NULL OR length(resource_type) BETWEEN 1 AND 64),
    ADD COLUMN resource_id uuid;

GRANT SELECT, INSERT, UPDATE ON certificates TO ocservia_app;
GRANT SELECT, INSERT, UPDATE ON artifact_operations TO ocservia_app;
GRANT SELECT, INSERT, UPDATE ON secret_provider_refs TO ocservia_app;
