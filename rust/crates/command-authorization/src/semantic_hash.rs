use ocservia_canonical_cbor::{Bytes, hash as cbor_hash};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, SealedSecretPurpose, SealedSecretV1, SemanticPayloadHashVersion,
    command_envelope,
};
use serde::Serialize;
use sha2::{Digest as _, Sha256};

use crate::AuthorizationError;

const DOMAIN_SEPARATOR_V2: &str = "ocservia.command.semantic-hash.cbor.v2\0";

#[derive(Serialize)]
struct CborEnvelope<'a, P: Serialize> {
    node_id: Bytes<'a>,
    expected_revision: u64,
    payload_kind: u32,
    payload: P,
}

#[derive(Serialize)]
struct CborEmpty {}

#[derive(Serialize)]
struct CborEcho<'a> {
    message: &'a str,
}

#[derive(Serialize)]
struct CborSession<'a> {
    session_id: &'a str,
    boot_id: &'a str,
}

#[derive(Serialize)]
struct CborIpBan<'a> {
    ip: &'a str,
}

#[derive(Serialize)]
struct CborSealedSecret<'a> {
    version: u32,
    purpose: u32,
    key_id: &'a str,
    ciphertext: Bytes<'a>,
}

#[derive(Serialize)]
struct CborUserWithSecret<'a> {
    username: &'a str,
    desired_revision: u64,
    sealed_password: CborSealedSecret<'a>,
}

#[derive(Serialize)]
struct CborUserToggle<'a> {
    username: &'a str,
    desired_revision: u64,
}

#[derive(Serialize)]
struct CborGroupApply<'a> {
    group_name: &'a str,
    members: &'a [String],
    desired_revision: u64,
}

#[derive(Serialize)]
struct CborConfigPlan<'a> {
    candidate_hash: Bytes<'a>,
    expected_revision: u64,
}

#[derive(Serialize)]
struct CborConfigApply<'a> {
    candidate_hash: Bytes<'a>,
    expected_current_hash: Bytes<'a>,
    desired_revision: u64,
}

#[derive(Serialize)]
struct CborCompleteConfigPlan<'a> {
    candidate_hash: Bytes<'a>,
}

#[derive(Serialize)]
struct CborCompleteConfigApply<'a> {
    candidate_hash: Bytes<'a>,
    materialized_hash: Bytes<'a>,
    expected_current_hash: Bytes<'a>,
    desired_revision: u64,
    plan_id: Bytes<'a>,
    plan_expires_at_seconds: i64,
    plan_expires_at_nanos: u32,
}

#[derive(Serialize)]
struct CborCertificateCsr<'a> {
    certificate_id: Bytes<'a>,
    common_name: &'a str,
    dns_names: &'a [String],
    key_bits: u32,
}

#[derive(Serialize)]
struct CborCertificateP12<'a> {
    certificate_id: Bytes<'a>,
    artifact_id: Bytes<'a>,
    certificate_chain_pem: Bytes<'a>,
    sealed_password: CborSealedSecret<'a>,
    certificate_version: u64,
    artifact_expires_at_seconds: i64,
    artifact_expires_at_nanos: u32,
}

#[derive(Serialize)]
struct CborCertificateRevoke<'a> {
    certificate_id: Bytes<'a>,
    reason: &'a str,
    certificate_version: u64,
}

#[derive(Serialize)]
struct CborAgentUpgrade<'a> {
    target_version: &'a str,
    architecture: &'a str,
    package_sha256: Bytes<'a>,
}

fn hash_envelope<P: Serialize>(
    envelope: &CommandEnvelope,
    payload_kind: u32,
    payload: P,
) -> Result<[u8; 32], AuthorizationError> {
    cbor_hash(
        DOMAIN_SEPARATOR_V2,
        &CborEnvelope {
            node_id: Bytes(&envelope.node_id),
            expected_revision: envelope.expected_revision,
            payload_kind,
            payload,
        },
    )
    .map_err(|_| invalid_claim("semantic_payload_cbor_error"))
}

/// Computes the canonical RFC 8949 Core Deterministic CBOR v2 semantic payload hash.
///
/// # Errors
///
/// Rejects malformed or unsupported typed payloads.
#[allow(clippy::too_many_lines)]
pub fn semantic_payload_hash_v2(
    envelope: &CommandEnvelope,
) -> Result<[u8; 32], AuthorizationError> {
    match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(_)) => {
            hash_envelope(envelope, 107_u32, CborEmpty {})
        }
        Some(command_envelope::Payload::SyntheticEcho(payload)) => hash_envelope(
            envelope,
            108_u32,
            CborEcho {
                message: &payload.message,
            },
        ),
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            validate_session_payload(&payload.session_id, &payload.boot_id)?;
            hash_envelope(
                envelope,
                100_u32,
                CborSession {
                    session_id: &payload.session_id,
                    boot_id: &payload.boot_id,
                },
            )
        }
        Some(command_envelope::Payload::ServiceReload(_)) => {
            hash_envelope(envelope, 105_u32, CborEmpty {})
        }
        Some(command_envelope::Payload::ConfigPlan(payload)) => {
            if payload.candidate_hash.len() != 32 {
                return Err(invalid_claim("candidate_hash_invalid"));
            }
            hash_envelope(
                envelope,
                103_u32,
                CborConfigPlan {
                    candidate_hash: Bytes(&payload.candidate_hash),
                    expected_revision: payload.expected_revision,
                },
            )
        }
        Some(command_envelope::Payload::ConfigApply(payload)) => {
            if payload.candidate_hash.len() != 32
                || payload.expected_current_hash.len() != 32
                || payload.desired_revision == 0
            {
                return Err(invalid_claim("config_apply_invalid"));
            }
            hash_envelope(
                envelope,
                104_u32,
                CborConfigApply {
                    candidate_hash: Bytes(&payload.candidate_hash),
                    expected_current_hash: Bytes(&payload.expected_current_hash),
                    desired_revision: payload.desired_revision,
                },
            )
        }
        Some(command_envelope::Payload::CompleteConfigPlan(payload)) => {
            let candidate = payload
                .candidate
                .as_ref()
                .ok_or_else(|| invalid_claim("complete_config_missing"))?;
            let canonical = ocservia_contracts::config_profile::canonical(candidate)
                .map_err(|_| invalid_claim("complete_config_invalid"))?;
            let digest = Sha256::digest(&canonical);
            if candidate.node_id != envelope.node_id || digest.as_slice() != payload.candidate_hash
            {
                return Err(invalid_claim("complete_config_identity_invalid"));
            }
            hash_envelope(
                envelope,
                129_u32,
                CborCompleteConfigPlan {
                    candidate_hash: Bytes(digest.as_slice()),
                },
            )
        }
        Some(command_envelope::Payload::CompleteConfigApply(payload)) => {
            let candidate = payload
                .candidate
                .as_ref()
                .ok_or_else(|| invalid_claim("complete_config_missing"))?;
            let canonical = ocservia_contracts::config_profile::canonical(candidate)
                .map_err(|_| invalid_claim("complete_config_invalid"))?;
            let digest = Sha256::digest(&canonical);
            let expiry = payload
                .plan_expires_at
                .as_ref()
                .ok_or_else(|| invalid_claim("complete_config_expiry_missing"))?;
            if candidate.node_id != envelope.node_id
                || digest.as_slice() != payload.candidate_hash
                || payload.materialized_hash.len() != 32
                || payload.expected_current_hash.len() != 32
                || payload.desired_revision <= candidate.expected_revision
                || payload.plan_id.len() != 16
                || !(-62_135_596_800..=253_402_300_799).contains(&expiry.seconds)
                || !(0..1_000_000_000).contains(&expiry.nanos)
            {
                return Err(invalid_claim("complete_config_identity_invalid"));
            }
            let plan_expires_at_nanos = u32::try_from(expiry.nanos)
                .map_err(|_| invalid_claim("complete_config_identity_invalid"))?;
            hash_envelope(
                envelope,
                130_u32,
                CborCompleteConfigApply {
                    candidate_hash: Bytes(digest.as_slice()),
                    materialized_hash: Bytes(&payload.materialized_hash),
                    expected_current_hash: Bytes(&payload.expected_current_hash),
                    desired_revision: payload.desired_revision,
                    plan_id: Bytes(&payload.plan_id),
                    plan_expires_at_seconds: expiry.seconds,
                    plan_expires_at_nanos,
                },
            )
        }
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.common_name.is_empty()
                || payload.dns_names.len() > 32
                || !matches!(payload.key_bits, 2048 | 3072 | 4096)
            {
                return Err(invalid_claim("certificate_csr_invalid"));
            }
            hash_envelope(
                envelope,
                117_u32,
                CborCertificateCsr {
                    certificate_id: Bytes(&payload.certificate_id),
                    common_name: &payload.common_name,
                    dns_names: &payload.dns_names,
                    key_bits: payload.key_bits,
                },
            )
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
                || payload.certificate_version == 0
                || !(0..=999_999_999).contains(&expires.nanos)
            {
                return Err(invalid_claim("certificate_p12_invalid"));
            }
            let sealed_password =
                validate_sealed_secret(secret, SealedSecretPurpose::CertificateP12Password)?;
            let artifact_expires_at_nanos = u32::try_from(expires.nanos)
                .map_err(|_| invalid_claim("certificate_p12_expiry_invalid"))?;
            hash_envelope(
                envelope,
                118_u32,
                CborCertificateP12 {
                    certificate_id: Bytes(&payload.certificate_id),
                    artifact_id: Bytes(&payload.artifact_id),
                    certificate_chain_pem: Bytes(&payload.certificate_chain_pem),
                    sealed_password,
                    certificate_version: payload.certificate_version,
                    artifact_expires_at_seconds: expires.seconds,
                    artifact_expires_at_nanos,
                },
            )
        }
        Some(command_envelope::Payload::CertificateRevoke(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.reason.is_empty()
                || payload.reason.len() > 128
                || payload.certificate_version == 0
            {
                return Err(invalid_claim("certificate_revoke_invalid"));
            }
            hash_envelope(
                envelope,
                119_u32,
                CborCertificateRevoke {
                    certificate_id: Bytes(&payload.certificate_id),
                    reason: &payload.reason,
                    certificate_version: payload.certificate_version,
                },
            )
        }
        Some(command_envelope::Payload::AgentUpgrade(payload)) => {
            if !ocservia_contracts::agent_upgrade::valid_target_version(&payload.target_version)
                || payload.package_sha256.len() != 32
                || !ocservia_contracts::agent_upgrade::valid_architecture(&payload.architecture)
            {
                return Err(invalid_claim("agent_upgrade_invalid"));
            }
            hash_envelope(
                envelope,
                128_u32,
                CborAgentUpgrade {
                    target_version: &payload.target_version,
                    architecture: &payload.architecture,
                    package_sha256: Bytes(&payload.package_sha256),
                },
            )
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            validate_session_payload(&payload.session_id, &payload.boot_id)?;
            hash_envelope(
                envelope,
                112_u32,
                CborSession {
                    session_id: &payload.session_id,
                    boot_id: &payload.boot_id,
                },
            )
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            hash_envelope(envelope, 113_u32, CborIpBan { ip: &payload.ip })
        }
        Some(command_envelope::Payload::UserCreate(payload)) => {
            validate_semantic_name(&payload.username)?;
            validate_semantic_revision(payload.desired_revision)?;
            let secret = payload
                .sealed_password_v1
                .as_ref()
                .ok_or_else(|| invalid_claim("sealed_secret_missing"))?;
            let sealed_password =
                validate_sealed_secret(secret, SealedSecretPurpose::UserPassword)?;
            hash_envelope(
                envelope,
                101_u32,
                CborUserWithSecret {
                    username: &payload.username,
                    desired_revision: payload.desired_revision,
                    sealed_password,
                },
            )
        }
        Some(command_envelope::Payload::UserDisable(payload)) => {
            validate_semantic_name(&payload.username)?;
            validate_semantic_revision(payload.desired_revision)?;
            hash_envelope(
                envelope,
                102_u32,
                CborUserToggle {
                    username: &payload.username,
                    desired_revision: payload.desired_revision,
                },
            )
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            validate_semantic_name(&payload.username)?;
            validate_semantic_revision(payload.desired_revision)?;
            let secret = payload
                .sealed_password_v1
                .as_ref()
                .ok_or_else(|| invalid_claim("sealed_secret_missing"))?;
            let sealed_password =
                validate_sealed_secret(secret, SealedSecretPurpose::UserPassword)?;
            hash_envelope(
                envelope,
                114_u32,
                CborUserWithSecret {
                    username: &payload.username,
                    desired_revision: payload.desired_revision,
                    sealed_password,
                },
            )
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            validate_semantic_name(&payload.group_name)?;
            validate_semantic_revision(payload.desired_revision)?;
            for member in &payload.members {
                validate_semantic_name(member)?;
            }
            hash_envelope(
                envelope,
                115_u32,
                CborGroupApply {
                    group_name: &payload.group_name,
                    members: &payload.members,
                    desired_revision: payload.desired_revision,
                },
            )
        }
        Some(command_envelope::Payload::UserEnable(payload)) => {
            validate_semantic_name(&payload.username)?;
            validate_semantic_revision(payload.desired_revision)?;
            hash_envelope(
                envelope,
                116_u32,
                CborUserToggle {
                    username: &payload.username,
                    desired_revision: payload.desired_revision,
                },
            )
        }
        _ => Err(AuthorizationError::PayloadUnsupported),
    }
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
    if version != SemanticPayloadHashVersion::V2 {
        return Err(AuthorizationError::ClaimsInvalid(
            "semantic_hash_version_unsupported",
        ));
    }
    let recomputed = semantic_payload_hash_v2(envelope)?;
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

fn validate_sealed_secret(
    secret: &SealedSecretV1,
    expected_purpose: SealedSecretPurpose,
) -> Result<CborSealedSecret<'_>, AuthorizationError> {
    if secret.version != 1
        || SealedSecretPurpose::try_from(secret.purpose).ok() != Some(expected_purpose)
        || secret.key_id.is_empty()
        || secret.key_id.len() > 128
        || secret.ciphertext.len() < 32
        || secret.ciphertext.len() > 16 * 1024
    {
        return Err(invalid_claim("sealed_secret_invalid"));
    }
    let purpose =
        u32::try_from(secret.purpose).map_err(|_| invalid_claim("sealed_secret_invalid"))?;
    Ok(CborSealedSecret {
        version: secret.version,
        purpose,
        key_id: &secret.key_id,
        ciphertext: Bytes(&secret.ciphertext),
    })
}

fn validate_session_payload(session_id: &str, boot_id: &str) -> Result<(), AuthorizationError> {
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
    Ok(())
}

fn validate_semantic_name(value: &str) -> Result<(), AuthorizationError> {
    if !ocservia_contracts::unix::is_valid_resource_name(value) {
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
