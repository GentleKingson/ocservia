CREATE TABLE privd_attestation_enrollment_credentials (
    id uuid PRIMARY KEY CHECK (uuid_extract_version(id) = 7),
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    secret_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(secret_sha256) = 32),
    controller_nonce bytea NOT NULL CHECK (octet_length(controller_nonce) = 32),
    credential_context_sha256 bytea NOT NULL CHECK (octet_length(credential_context_sha256) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_by_identity_id uuid NOT NULL REFERENCES identities(id),
    created_by_session_id uuid NOT NULL REFERENCES auth_sessions(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);

CREATE INDEX privd_attestation_credentials_node_active_idx
    ON privd_attestation_enrollment_credentials(node_id, expires_at)
    WHERE consumed_at IS NULL;

CREATE TABLE node_privd_attestation_keys (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    key_id text NOT NULL CHECK (key_id ~ '^ed25519-sha256:[0-9a-f]{64}$'),
    algorithm text NOT NULL CHECK (algorithm = 'ed25519'),
    public_key bytea NOT NULL CHECK (octet_length(public_key) = 32),
    state text NOT NULL CHECK (state IN ('pending','active','revoked')),
    created_at timestamptz NOT NULL,
    approved_at timestamptz NOT NULL,
    activated_at timestamptz NOT NULL,
    valid_until timestamptz,
    revoked_at timestamptz,
    predecessor_key_id text,
    successor_key_id text,
    registration_credential_id uuid NOT NULL UNIQUE REFERENCES privd_attestation_enrollment_credentials(id),
    PRIMARY KEY (node_id, key_id),
    UNIQUE (key_id),
    UNIQUE (public_key),
    CHECK (valid_until IS NULL OR valid_until >= activated_at),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
    CHECK (predecessor_key_id IS NULL OR predecessor_key_id <> key_id),
    CHECK (successor_key_id IS NULL OR successor_key_id <> key_id)
);

CREATE INDEX node_privd_attestation_keys_active_idx
    ON node_privd_attestation_keys(node_id, state, activated_at)
    WHERE state = 'active';

ALTER TABLE agent_command_results
    ADD COLUMN receipt_verification_status text NOT NULL DEFAULT 'legacy'
        CHECK (receipt_verification_status IN ('legacy','not_required','verified','missing','invalid','unknown_key','revoked_key')),
    ADD COLUMN receipt_failure_reason text
        CHECK (receipt_failure_reason IS NULL OR length(receipt_failure_reason) BETWEEN 1 AND 64),
    ADD COLUMN privd_attestation_key_id text,
    ADD COLUMN effect_record_id bytea,
    ADD COLUMN effect_sequence bigint,
    ADD COLUMN receipt_sha256 bytea,
    ADD COLUMN privileged_result_proof bytea,
    ADD CONSTRAINT agent_command_results_receipt_fields_check CHECK (
        (receipt_verification_status = 'verified'
            AND receipt_failure_reason IS NULL
            AND privd_attestation_key_id IS NOT NULL
            AND effect_record_id IS NOT NULL
            AND octet_length(effect_record_id) BETWEEN 16 AND 32
            AND effect_sequence > 0
            AND octet_length(receipt_sha256) = 32
            AND octet_length(privileged_result_proof) BETWEEN 1 AND 65536)
        OR receipt_verification_status <> 'verified'
    );

CREATE INDEX agent_command_results_receipt_idx
    ON agent_command_results(command_id, effect_record_id, effect_sequence)
    WHERE receipt_verification_status = 'verified';

CREATE UNIQUE INDEX agent_command_results_effect_receipt_unique_idx
    ON agent_command_results(privd_attestation_key_id, effect_record_id, effect_sequence)
    WHERE receipt_verification_status = 'verified';

ALTER TABLE certificates
    ADD COLUMN csr_receipt_legacy boolean NOT NULL DEFAULT false,
    ADD COLUMN csr_receipt_verified_at timestamptz,
    ADD COLUMN csr_receipt_sha256 bytea,
    ADD COLUMN csr_privd_attestation_key_id text,
    ADD COLUMN csr_effect_record_id bytea,
    ADD COLUMN csr_der_sha256 bytea,
    ADD COLUMN csr_requested_subject_sha256 bytea,
    ADD COLUMN issue_certificate_version bigint CHECK (issue_certificate_version > 0);

ALTER TABLE certificates
    ADD CONSTRAINT certificates_csr_privd_key_fk
    FOREIGN KEY (node_id, csr_privd_attestation_key_id)
    REFERENCES node_privd_attestation_keys(node_id, key_id);

UPDATE certificates SET csr_receipt_legacy = true
WHERE state IN ('csr_ready','signing','signer_unavailable','issued','expiring','expired','revoking','revocation_unknown','revoked')
  AND csr_receipt_verified_at IS NULL;

ALTER TABLE certificates ADD CONSTRAINT certificates_csr_receipt_check CHECK (
    (state IN ('csr_ready','signing','signer_unavailable','issued','expiring','expired','revoking','revocation_unknown','revoked')
        AND csr_receipt_verified_at IS NOT NULL
        AND octet_length(csr_receipt_sha256) = 32
        AND csr_privd_attestation_key_id IS NOT NULL
        AND octet_length(csr_effect_record_id) BETWEEN 16 AND 32
        AND octet_length(csr_der_sha256) = 32
        AND octet_length(csr_requested_subject_sha256) = 32)
    OR csr_receipt_legacy
    OR state IN ('csr_pending','failed','unknown')
);

COMMENT ON TABLE node_privd_attestation_keys IS
    'Root-authenticated per-node privd Ed25519 trust anchors with bounded rotation overlap.';
COMMENT ON COLUMN agent_command_results.receipt_verification_status IS
    'Fail-closed Controller verification outcome for privileged terminal result evidence.';
