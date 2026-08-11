DROP INDEX artifact_operations_active_grant_idx;
UPDATE artifact_operations
SET state = 'expired', lease_until = NULL, updated_at = now()
WHERE state IN ('pending','ready','leased','revoked');
ALTER TABLE artifact_operations
    DROP CONSTRAINT artifact_operations_grant_check,
    DROP CONSTRAINT artifact_operations_state_check,
    DROP CONSTRAINT artifact_operations_certificate_version_check,
    ADD CONSTRAINT artifact_operations_state_check CHECK (state IN ('pending','ready','leased','consumed','expired','failed')),
    DROP COLUMN active_grant_expires_at,
    DROP COLUMN active_grant_subject,
    DROP COLUMN active_grant_id,
    DROP COLUMN certificate_version;
DROP TABLE node_sealing_keys;
