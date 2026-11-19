use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, SealedSecretPurpose, SealedSecretV1, SealedSecretVersion,
    SemanticPayloadHashVersion, command_envelope,
};
use sha2::{Digest as _, Sha256};

use crate::AuthorizationError;

/// Computes the frozen v1 semantic payload hash without serializing Protobuf.
///
/// # Errors
///
/// Rejects malformed or unsupported typed payloads.
pub fn semantic_payload_hash_v1(
    envelope: &CommandEnvelope,
) -> Result<[u8; 32], AuthorizationError> {
    canonical_semantic_payload_hash(envelope, SemanticPayloadHashVersion::V1)
}

/// Computes the v2 semantic payload hash, including the `ConfigPlan` desired
/// revision added by protocol 1.1.
///
/// # Errors
///
/// Rejects malformed or unsupported typed payloads.
pub fn semantic_payload_hash_v2(
    envelope: &CommandEnvelope,
) -> Result<[u8; 32], AuthorizationError> {
    canonical_semantic_payload_hash(envelope, SemanticPayloadHashVersion::V2)
}

/// Recomputes and compares the semantic payload hash selected by the envelope.
///
/// # Errors
///
/// Rejects unsupported versions, malformed typed payloads, and mismatches.
pub fn verify_semantic_payload_hash(
    envelope: &CommandEnvelope,
) -> Result<[u8; 32], AuthorizationError> {
    let version = SemanticPayloadHashVersion::try_from(envelope.semantic_payload_hash_version)
        .unwrap_or(SemanticPayloadHashVersion::Unspecified);
    let recomputed = match version {
        SemanticPayloadHashVersion::V1 => semantic_payload_hash_v1(envelope)?,
        SemanticPayloadHashVersion::V2 => semantic_payload_hash_v2(envelope)?,
        SemanticPayloadHashVersion::Unspecified => {
            return Err(AuthorizationError::ClaimsInvalid(
                "semantic_hash_version_unsupported",
            ));
        }
    };
    let expected: [u8; 32] = envelope
        .semantic_payload_sha256
        .as_slice()
        .try_into()
        .map_err(|_| AuthorizationError::ClaimsInvalid("semantic_payload_hash_missing"))?;
    if recomputed != expected {
        return Err(AuthorizationError::ClaimsInvalid(
            "semantic_payload_hash_mismatch",
        ));
    }
    Ok(recomputed)
}

fn start_semantic_hash(
    envelope: &CommandEnvelope,
    version: SemanticPayloadHashVersion,
    payload_kind: u32,
) -> Result<Sha256, AuthorizationError> {
    let domain: &[u8] = match version {
        SemanticPayloadHashVersion::V1 => b"ocservia.command.semantic-hash.v1\0",
        SemanticPayloadHashVersion::V2 => b"ocservia.command.semantic-hash.v2\0",
        SemanticPayloadHashVersion::Unspecified => {
            return Err(invalid_claim("semantic_hash_version_unsupported"));
        }
    };
    let mut hash = Sha256::new();
    hash.update(domain);
    hash.update(&envelope.node_id);
    hash.update(envelope.expected_revision.to_be_bytes());
    hash.update(payload_kind.to_be_bytes());
    Ok(hash)
}

#[allow(clippy::too_many_lines)]
fn canonical_semantic_payload_hash(
    envelope: &CommandEnvelope,
    version: SemanticPayloadHashVersion,
) -> Result<[u8; 32], AuthorizationError> {
    let hash = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(_)) => {
            start_semantic_hash(envelope, version, 107_u32)?
        }
        Some(command_envelope::Payload::SyntheticEcho(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 108_u32)?;
            update_semantic_bytes(&mut hash, payload.message.as_bytes())?;
            hash
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 100_u32)?;
            update_session_payload(&mut hash, &payload.session_id, &payload.boot_id)?;
            hash
        }
        Some(command_envelope::Payload::ServiceReload(_)) => {
            start_semantic_hash(envelope, version, 105_u32)?
        }
        Some(command_envelope::Payload::ConfigPlan(payload)) => {
            if payload.candidate_hash.len() != 32 {
                return Err(invalid_claim("candidate_hash_invalid"));
            }
            let mut hash = start_semantic_hash(envelope, version, 103_u32)?;
            hash.update(&payload.candidate_hash);
            if version == SemanticPayloadHashVersion::V2 {
                hash.update(payload.expected_revision.to_be_bytes());
            }
            hash
        }
        Some(command_envelope::Payload::ConfigApply(payload)) => {
            if payload.candidate_hash.len() != 32
                || payload.expected_current_hash.len() != 32
                || payload.desired_revision == 0
            {
                return Err(invalid_claim("config_apply_invalid"));
            }
            let mut hash = start_semantic_hash(envelope, version, 104_u32)?;
            hash.update(&payload.candidate_hash);
            hash.update(&payload.expected_current_hash);
            hash.update(payload.desired_revision.to_be_bytes());
            hash
        }
        Some(command_envelope::Payload::CompleteConfigPlan(payload)) => {
            let candidate = payload
                .candidate
                .as_ref()
                .ok_or_else(|| invalid_claim("complete_config_missing"))?;
            let canonical = ocservia_contracts::config_profile::canonical(candidate)
                .map_err(|_| invalid_claim("complete_config_invalid"))?;
            if version != SemanticPayloadHashVersion::V2
                || candidate.node_id != envelope.node_id
                || Sha256::digest(&canonical).as_slice() != payload.candidate_hash
            {
                return Err(invalid_claim("complete_config_identity_invalid"));
            }
            let mut hash = start_semantic_hash(envelope, version, 129_u32)?;
            hash.update(&payload.candidate_hash);
            hash
        }
        Some(command_envelope::Payload::CompleteConfigApply(payload)) => {
            let candidate = payload
                .candidate
                .as_ref()
                .ok_or_else(|| invalid_claim("complete_config_missing"))?;
            let canonical = ocservia_contracts::config_profile::canonical(candidate)
                .map_err(|_| invalid_claim("complete_config_invalid"))?;
            let expiry = payload
                .plan_expires_at
                .as_ref()
                .ok_or_else(|| invalid_claim("complete_config_expiry_missing"))?;
            if version != SemanticPayloadHashVersion::V2
                || candidate.node_id != envelope.node_id
                || Sha256::digest(&canonical).as_slice() != payload.candidate_hash
                || payload.materialized_hash.len() != 32
                || payload.expected_current_hash.len() != 32
                || payload.desired_revision <= candidate.expected_revision
                || payload.plan_id.len() != 16
                || !(-62_135_596_800..=253_402_300_799).contains(&expiry.seconds)
                || !(0..1_000_000_000).contains(&expiry.nanos)
                || !((envelope.approval_id.is_empty()
                    && envelope.approval_request_sha256.is_empty())
                    || (envelope.approval_id.len() == 16
                        && envelope.approval_request_sha256.len() == 32))
            {
                return Err(invalid_claim("complete_config_identity_invalid"));
            }
            let mut hash = start_semantic_hash(envelope, version, 130_u32)?;
            hash.update(&payload.candidate_hash);
            hash.update(&payload.materialized_hash);
            hash.update(&payload.expected_current_hash);
            hash.update(payload.desired_revision.to_be_bytes());
            hash.update(&payload.plan_id);
            hash.update(expiry.seconds.to_be_bytes());
            hash.update(expiry.nanos.to_be_bytes());
            hash.update(&envelope.approval_id);
            hash.update(&envelope.approval_request_sha256);
            hash
        }
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.common_name.is_empty()
                || payload.dns_names.len() > 32
                || !matches!(payload.key_bits, 2048 | 3072 | 4096)
            {
                return Err(invalid_claim("certificate_csr_invalid"));
            }
            let mut hash = start_semantic_hash(envelope, version, 117_u32)?;
            update_semantic_bytes(&mut hash, payload.common_name.as_bytes())?;
            for dns_name in &payload.dns_names {
                update_semantic_bytes(&mut hash, dns_name.as_bytes())?;
            }
            update_semantic_bytes(&mut hash, &payload.certificate_id)?;
            hash.update(u64::from(payload.key_bits).to_be_bytes());
            hash
        }
        Some(command_envelope::Payload::CertificateP12(payload)) => {
            let secret = payload
                .sealed_password_v1
                .as_ref()
                .ok_or_else(|| invalid_claim("certificate_p12_secret_missing"))?;
            let expires = payload
                .artifact_expires_at
                .as_ref()
                .ok_or_else(|| invalid_claim("certificate_p12_expiry_missing"))?;
            if payload.certificate_id.len() != 16
                || payload.artifact_id.len() != 16
                || payload.certificate_chain_pem.len() < 64
                || payload.certificate_chain_pem.len() > 256 * 1024
                || !payload.sealed_password.is_empty()
                || !payload.secret_key_id.is_empty()
                || payload.certificate_version == 0
                || !(0..=999_999_999).contains(&expires.nanos)
            {
                return Err(invalid_claim("certificate_p12_invalid"));
            }
            let (secret_version, secret_purpose) =
                validate_sealed_secret(secret, SealedSecretPurpose::CertificateP12Password)?;
            let expires_nanos = u32::try_from(expires.nanos)
                .map_err(|_| invalid_claim("certificate_p12_expiry_invalid"))?;
            let data_len = payload
                .certificate_id
                .len()
                .checked_add(payload.artifact_id.len())
                .and_then(|n| n.checked_add(payload.certificate_chain_pem.len()))
                .and_then(|n| n.checked_add(8))
                .and_then(|n| n.checked_add(secret.ciphertext.len()))
                .and_then(|n| n.checked_add(12))
                .and_then(|n| u32::try_from(n).ok())
                .ok_or_else(|| invalid_claim("payload_size_invalid"))?;
            let mut hash = start_semantic_hash(envelope, version, 118_u32)?;
            update_semantic_bytes(&mut hash, secret.key_id.as_bytes())?;
            hash.update(data_len.to_be_bytes());
            hash.update(&payload.certificate_id);
            hash.update(&payload.artifact_id);
            hash.update(&payload.certificate_chain_pem);
            hash.update(secret_version.to_be_bytes());
            hash.update(secret_purpose.to_be_bytes());
            hash.update(&secret.ciphertext);
            hash.update(expires.seconds.to_be_bytes());
            hash.update(expires_nanos.to_be_bytes());
            hash.update(payload.certificate_version.to_be_bytes());
            hash
        }
        Some(command_envelope::Payload::CertificateRevoke(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.reason.is_empty()
                || payload.reason.len() > 128
                || payload.certificate_version == 0
            {
                return Err(invalid_claim("certificate_revoke_invalid"));
            }
            let mut hash = start_semantic_hash(envelope, version, 119_u32)?;
            update_semantic_bytes(&mut hash, payload.reason.as_bytes())?;
            update_semantic_bytes(&mut hash, &payload.certificate_id)?;
            hash.update(payload.certificate_version.to_be_bytes());
            hash
        }
        Some(command_envelope::Payload::AgentUpgrade(payload)) => {
            if !ocservia_contracts::agent_upgrade::valid_target_version(&payload.target_version)
                || payload.package_sha256.len() != 32
                || !ocservia_contracts::agent_upgrade::valid_architecture(&payload.architecture)
            {
                return Err(invalid_claim("agent_upgrade_invalid"));
            }
            let mut hash = start_semantic_hash(envelope, version, 128_u32)?;
            update_semantic_bytes(&mut hash, payload.target_version.as_bytes())?;
            update_semantic_bytes(&mut hash, payload.architecture.as_bytes())?;
            update_semantic_bytes(&mut hash, &payload.package_sha256)?;
            hash.update(0_u64.to_be_bytes());
            hash
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 112_u32)?;
            update_session_payload(&mut hash, &payload.session_id, &payload.boot_id)?;
            hash
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 113_u32)?;
            update_semantic_bytes(&mut hash, payload.ip.as_bytes())?;
            hash
        }
        Some(command_envelope::Payload::UserCreate(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 101_u32)?;
            update_secret_payload(
                &mut hash,
                &payload.username,
                payload.sealed_password_v1.as_ref(),
                &payload.sealed_password,
                &payload.secret_key_id,
                payload.desired_revision,
            )?;
            hash
        }
        Some(command_envelope::Payload::UserDisable(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 102_u32)?;
            update_named_payload(&mut hash, &payload.username, &[], payload.desired_revision)?;
            hash
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 114_u32)?;
            update_secret_payload(
                &mut hash,
                &payload.username,
                payload.sealed_password_v1.as_ref(),
                &payload.sealed_password,
                &payload.secret_key_id,
                payload.desired_revision,
            )?;
            hash
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 115_u32)?;
            update_named_payload(
                &mut hash,
                &payload.group_name,
                &payload.members,
                payload.desired_revision,
            )?;
            hash
        }
        Some(command_envelope::Payload::UserEnable(payload)) => {
            let mut hash = start_semantic_hash(envelope, version, 116_u32)?;
            update_named_payload(&mut hash, &payload.username, &[], payload.desired_revision)?;
            hash
        }
        _ => return Err(AuthorizationError::PayloadUnsupported),
    };
    Ok(hash.finalize().into())
}

fn update_named_payload(
    hash: &mut Sha256,
    name: &str,
    values: &[String],
    revision: u64,
) -> Result<(), AuthorizationError> {
    validate_semantic_name(name)?;
    validate_semantic_revision(revision)?;
    update_semantic_bytes(hash, name.as_bytes())?;
    for value in values {
        validate_semantic_name(value)?;
        update_semantic_bytes(hash, value.as_bytes())?;
    }
    hash.update(0_u32.to_be_bytes());
    hash.update(revision.to_be_bytes());
    Ok(())
}

fn update_secret_payload(
    hash: &mut Sha256,
    name: &str,
    secret: Option<&SealedSecretV1>,
    legacy_ciphertext: &[u8],
    legacy_key_id: &str,
    revision: u64,
) -> Result<(), AuthorizationError> {
    validate_semantic_name(name)?;
    validate_semantic_revision(revision)?;
    if !legacy_ciphertext.is_empty() || !legacy_key_id.is_empty() {
        return Err(invalid_claim("sealed_secret_invalid"));
    }
    let secret = secret.ok_or_else(|| invalid_claim("sealed_secret_missing"))?;
    let (secret_version, secret_purpose) =
        validate_sealed_secret(secret, SealedSecretPurpose::UserPassword)?;
    let secret_bytes_len = secret
        .ciphertext
        .len()
        .checked_add(8)
        .and_then(|n| u32::try_from(n).ok())
        .ok_or_else(|| invalid_claim("payload_size_invalid"))?;
    update_semantic_bytes(hash, name.as_bytes())?;
    update_semantic_bytes(hash, secret.key_id.as_bytes())?;
    hash.update(secret_bytes_len.to_be_bytes());
    hash.update(secret_version.to_be_bytes());
    hash.update(secret_purpose.to_be_bytes());
    hash.update(&secret.ciphertext);
    hash.update(revision.to_be_bytes());
    Ok(())
}

fn validate_sealed_secret(
    secret: &SealedSecretV1,
    expected_purpose: SealedSecretPurpose,
) -> Result<(u32, u32), AuthorizationError> {
    if SealedSecretVersion::try_from(secret.version).ok() != Some(SealedSecretVersion::V1)
        || SealedSecretPurpose::try_from(secret.purpose).ok() != Some(expected_purpose)
        || secret.key_id.is_empty()
        || secret.key_id.len() > 128
        || secret.ciphertext.len() < 32
        || secret.ciphertext.len() > 16 * 1024
    {
        return Err(invalid_claim("sealed_secret_invalid"));
    }
    let version =
        u32::try_from(secret.version).map_err(|_| invalid_claim("sealed_secret_invalid"))?;
    let purpose =
        u32::try_from(secret.purpose).map_err(|_| invalid_claim("sealed_secret_invalid"))?;
    Ok((version, purpose))
}

fn update_session_payload(
    hash: &mut Sha256,
    session_id: &str,
    boot_id: &str,
) -> Result<(), AuthorizationError> {
    let bytes = session_id.as_bytes();
    if bytes.is_empty()
        || !matches!(bytes[0], b'1'..=b'9')
        || !bytes.iter().all(u8::is_ascii_digit)
        || session_id.parse::<u64>().is_err()
        || boot_id.is_empty()
        || boot_id.len() > 64
    {
        return Err(invalid_claim("session_id_invalid"));
    }
    update_semantic_bytes(hash, bytes)?;
    update_semantic_bytes(hash, boot_id.as_bytes())?;
    Ok(())
}

fn update_semantic_bytes(hash: &mut Sha256, value: &[u8]) -> Result<(), AuthorizationError> {
    hash.update(
        u32::try_from(value.len())
            .map_err(|_| invalid_claim("payload_size_invalid"))?
            .to_be_bytes(),
    );
    hash.update(value);
    Ok(())
}

fn validate_semantic_name(value: &str) -> Result<(), AuthorizationError> {
    if value.is_empty()
        || value.len() > 64
        || !value.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_alphanumeric() || (index > 0 && matches!(byte, b'_' | b'.' | b'-'))
        })
    {
        return Err(invalid_claim("name_invalid"));
    }
    Ok(())
}

fn validate_semantic_revision(value: u64) -> Result<(), AuthorizationError> {
    if value == 0 {
        return Err(invalid_claim("revision_invalid"));
    }
    Ok(())
}

const fn invalid_claim(code: &'static str) -> AuthorizationError {
    AuthorizationError::ClaimsInvalid(code)
}
