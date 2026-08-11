ALTER TABLE artifact_operations
    ADD COLUMN certificate_version bigint,
    ADD COLUMN active_grant_id uuid,
    ADD COLUMN active_grant_subject text,
    ADD COLUMN active_grant_expires_at timestamptz,
    ADD COLUMN consume_grant bytea,
    ADD COLUMN consume_sha256 bytea,
    ADD COLUMN consume_size bigint,
    ADD COLUMN consume_actor_id uuid,
    ADD COLUMN consume_session_id uuid,
    ADD COLUMN consume_request_id text;

UPDATE artifact_operations a
SET certificate_version = c.version
FROM certificates c
WHERE c.id = a.certificate_id;

-- Pre-v1 grants have no Controller signature or root-side lifecycle evidence.
-- Expire them explicitly instead of carrying an unsigned lease across the
-- protocol boundary or failing the new leased-state constraint.
UPDATE artifact_operations
SET state = 'expired', lease_until = NULL, updated_at = now()
WHERE state IN ('pending','ready','leased');

ALTER TABLE artifact_operations
    ALTER COLUMN certificate_version SET NOT NULL,
    ADD CONSTRAINT artifact_operations_certificate_version_check CHECK (certificate_version > 0),
    DROP CONSTRAINT artifact_operations_state_check,
    ADD CONSTRAINT artifact_operations_state_check CHECK (state IN ('pending','ready','leased','consuming','consumed','expired','revoked','failed')),
    ADD CONSTRAINT artifact_operations_grant_check CHECK (
        (state = 'leased' AND active_grant_id IS NOT NULL AND active_grant_subject IS NOT NULL AND active_grant_expires_at IS NOT NULL)
        OR state <> 'leased'
    ),
    ADD CONSTRAINT artifact_operations_consume_check CHECK (
        (state = 'consuming'
            AND consume_grant IS NOT NULL AND octet_length(consume_grant) BETWEEN 1 AND 4096
            AND consume_sha256 IS NOT NULL AND octet_length(consume_sha256) = 32
            AND consume_size > 0
            AND consume_actor_id IS NOT NULL
            AND consume_session_id IS NOT NULL
            AND consume_request_id IS NOT NULL AND length(consume_request_id) BETWEEN 1 AND 128)
        OR state <> 'consuming'
    );

DROP INDEX artifact_operations_one_live_certificate_idx;
CREATE UNIQUE INDEX artifact_operations_one_live_certificate_idx
    ON artifact_operations(certificate_id)
    WHERE state IN ('pending','ready','leased','consuming');

CREATE UNIQUE INDEX artifact_operations_active_grant_idx
    ON artifact_operations(active_grant_id)
    WHERE active_grant_id IS NOT NULL;

COMMENT ON COLUMN artifact_operations.certificate_version IS
    'Certificate version bound into the Controller-signed ArtifactGrantV1 and local artifact evidence.';

CREATE TABLE node_sealing_keys (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    purpose smallint NOT NULL CHECK (purpose IN (1,2)),
    version smallint NOT NULL CHECK (version=1),
    key_id text NOT NULL CHECK (key_id ~ '^[A-Za-z0-9_.-]{1,128}$'),
    public_key_sha256 bytea NOT NULL CHECK (octet_length(public_key_sha256)=32),
    created_at timestamptz NOT NULL,
    PRIMARY KEY(node_id,purpose),
    UNIQUE(node_id,key_id),
    UNIQUE(node_id,public_key_sha256)
);

GRANT SELECT, INSERT ON node_sealing_keys TO ocservia_app;
