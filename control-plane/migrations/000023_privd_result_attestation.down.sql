DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM node_privd_attestation_keys)
       OR EXISTS (SELECT 1 FROM agent_command_results WHERE receipt_verification_status = 'verified')
       OR EXISTS (SELECT 1 FROM certificates WHERE csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy) THEN
        RAISE EXCEPTION 'cannot remove privd attestation while trusted keys or verified receipts exist';
    END IF;
END;
$$;

ALTER TABLE certificates
    DROP CONSTRAINT IF EXISTS certificates_csr_receipt_check,
    DROP CONSTRAINT IF EXISTS certificates_csr_privd_key_fk,
    DROP COLUMN IF EXISTS issue_certificate_version,
    DROP COLUMN IF EXISTS csr_requested_subject_sha256,
    DROP COLUMN IF EXISTS csr_der_sha256,
    DROP COLUMN IF EXISTS csr_effect_record_id,
    DROP COLUMN IF EXISTS csr_privd_attestation_key_id,
    DROP COLUMN IF EXISTS csr_receipt_sha256,
    DROP COLUMN IF EXISTS csr_receipt_verified_at,
    DROP COLUMN IF EXISTS csr_receipt_legacy;

DROP INDEX IF EXISTS agent_command_results_effect_receipt_unique_idx;
DROP INDEX IF EXISTS agent_command_results_receipt_idx;
ALTER TABLE agent_command_results
    DROP CONSTRAINT IF EXISTS agent_command_results_receipt_fields_check,
    DROP COLUMN IF EXISTS privileged_result_proof,
    DROP COLUMN IF EXISTS receipt_sha256,
    DROP COLUMN IF EXISTS effect_sequence,
    DROP COLUMN IF EXISTS effect_record_id,
    DROP COLUMN IF EXISTS privd_attestation_key_id,
    DROP COLUMN IF EXISTS receipt_failure_reason,
    DROP COLUMN IF EXISTS receipt_verification_status;

DROP TABLE IF EXISTS node_privd_attestation_keys;
DROP TABLE IF EXISTS privd_attestation_enrollment_credentials;
