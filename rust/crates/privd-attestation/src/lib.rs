//! Canonical root-privilege result receipts encoded with RFC 8949 Core Deterministic CBOR.

#![forbid(unsafe_code)]

use std::fmt;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Write as _};
use std::os::unix::fs::{MetadataExt as _, OpenOptionsExt as _, PermissionsExt as _};
use std::path::Path;

use ed25519_dalek::{Signature, Signer as _, SigningKey, VerifyingKey};
use ocservia_canonical_cbor::Bytes;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgradeOutcomeState, AgentUpgradeResultProof, AgentUpgradeScheduledResult,
    CertificateArtifactResult, CertificateCsrResult, CertificateRevokeResult, CommandResultState,
    ConfigApplyResult, ConfigPlanResult, PrivdAttestationRegistrationV1,
    PrivdCertificateReceiptBindingV1, PrivdResultReceiptV1, PrivilegedCommandKind,
    PrivilegedResultKind, PrivilegedResultProof, SemanticPayloadHashVersion,
};
use prost::Message as _;
use serde::Serialize;
use sha2::{Digest as _, Sha256};

const RECEIPT_DOMAIN: &str = "ocservia/privd-result-receipt/cbor.v1\0";
const RESULT_PAYLOAD_DOMAIN: &str = "ocservia/privd-result-payload/cbor.v1\0";
const REGISTRATION_DOMAIN: &str = "ocservia/privd-attestation-registration/cbor.v1\0";
const UPGRADE_RESULT_DOMAIN: &str = "ocservia/privd-upgrade-result/cbor.v1\0";
const KEY_ID_PREFIX: &str = "ed25519-sha256:";
const MAX_CANONICAL_BYTES: usize = 2048;
const MAX_ERROR_CODE_BYTES: usize = 128;
const PRIVATE_KEY_BYTES: usize = 32;

#[derive(Serialize)]
struct CborCertificateBinding<'a> {
    certificate_id: Bytes<'a>,
    csr_der_sha256: Bytes<'a>,
    public_key_sha256: Bytes<'a>,
}

#[derive(Serialize)]
struct CborReceiptV1<'a> {
    receipt_version: u32,
    node_id: Bytes<'a>,
    privd_attestation_key_id: &'a str,
    command_id: Bytes<'a>,
    operation_id: Bytes<'a>,
    idempotency_key: Bytes<'a>,
    semantic_payload_hash_version: u32,
    semantic_payload_sha256: Bytes<'a>,
    command_kind: u32,
    result_kind: u32,
    terminal_state: u32,
    result_cbor_sha256: Bytes<'a>,
    error_code: &'a str,
    effect_sequence: u64,
    accepted_at_seconds: i64,
    accepted_at_nanos: u32,
    completed_at_seconds: i64,
    completed_at_nanos: u32,
    replayed: bool,
    certificate: Option<CborCertificateBinding<'a>>,
}

#[derive(Serialize)]
struct CborRegistrationV1<'a> {
    version: u32,
    node_id: Bytes<'a>,
    privd_attestation_key_id: &'a str,
    public_key: Bytes<'a>,
    controller_nonce: Bytes<'a>,
    credential_context_sha256: Bytes<'a>,
}

#[derive(Serialize)]
struct CborUpgradeResultProofV1<'a> {
    version: u32,
    node_id: Bytes<'a>,
    privd_attestation_key_id: &'a str,
    operation_id: Bytes<'a>,
    target_version: &'a str,
    package_sha256: Bytes<'a>,
    state: u32,
    completed_unix_ms: u64,
    result_sha256: Bytes<'a>,
    attested_unix_ms: u64,
}

#[derive(Serialize)]
struct CborResultPayloadEnvelope<P: Serialize> {
    result_kind: u32,
    payload: P,
}

#[derive(Serialize)]
struct CborEmptyResult {}

#[derive(Serialize)]
struct CborConfigPlanResult<'a> {
    candidate_hash: Bytes<'a>,
    diff_redacted: &'a str,
    warnings: &'a [String],
    current_unchanged: bool,
    staging_cleaned: bool,
    current_hash: Bytes<'a>,
    materialized_hash: Bytes<'a>,
}

#[derive(Serialize)]
struct CborConfigApplyResult<'a> {
    candidate_hash: Bytes<'a>,
    previous_hash: Bytes<'a>,
    observed_hash: Bytes<'a>,
    applied_revision: u64,
    healthy: bool,
    rolled_back: bool,
    failed_critical: bool,
    failure_code: &'a str,
}

#[derive(Serialize)]
struct CborCertificateCsrResult<'a> {
    certificate_id: Bytes<'a>,
    csr_der: Bytes<'a>,
    public_key_sha256: Bytes<'a>,
}

#[derive(Serialize)]
struct CborCertificateArtifactResult<'a> {
    certificate_id: Bytes<'a>,
    artifact_id: Bytes<'a>,
    artifact_sha256: Bytes<'a>,
    artifact_size: u64,
}

#[derive(Serialize)]
struct CborCertificateRevokeResult<'a> {
    certificate_id: Bytes<'a>,
    key_removed: bool,
}

#[derive(Serialize)]
struct CborAgentUpgradeScheduledResult<'a> {
    operation_id: Bytes<'a>,
    target_version: &'a str,
    package_sha256: Bytes<'a>,
}

/// Loads an existing strict owner-only key, or atomically creates one.
///
/// The caller selects the path; key rotation uses a new root-controlled path
/// and Controller overlap, never an Agent-requested in-place replacement.
///
/// # Errors
///
/// Returns an error when the parent or key metadata violates the root-only
/// boundary, random key persistence fails, or an existing key is malformed.
pub fn load_or_create_signing_key(path: &Path, expected_uid: u32) -> io::Result<SigningKey> {
    validate_key_parent(path, expected_uid)?;
    let parent = path
        .parent()
        .ok_or_else(|| invalid_key("attestation key parent missing"))?;
    match load_signing_key(path, expected_uid) {
        Ok(key) => return Ok(key),
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }

    let mut secret = [0_u8; PRIVATE_KEY_BYTES];
    rand::fill(&mut secret);
    let suffix: [u8; 8] = rand::random();
    let file_name = path
        .file_name()
        .and_then(|value| value.to_str())
        .ok_or_else(|| invalid_key("attestation key file name invalid"))?;
    let pending = path.with_file_name(format!(".{file_name}.pending-{}", hex::encode(suffix)));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&pending)?;
    let create_result: io::Result<()> = (|| {
        file.write_all(&secret)?;
        file.sync_all()?;
        rustix::fs::renameat_with(
            rustix::fs::CWD,
            &pending,
            rustix::fs::CWD,
            path,
            rustix::fs::RenameFlags::NOREPLACE,
        )?;
        File::open(parent)?.sync_all()?;
        Ok(())
    })();
    if create_result.is_err() {
        let _ = fs::remove_file(&pending);
        create_result?;
    }
    load_signing_key(path, expected_uid)
}

fn load_signing_key(path: &Path, expected_uid: u32) -> io::Result<SigningKey> {
    let metadata = fs::symlink_metadata(path)?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != expected_uid
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o777 != 0o600
        || metadata.len() != PRIVATE_KEY_BYTES as u64
    {
        return Err(invalid_key("attestation key metadata invalid"));
    }
    let bytes: [u8; PRIVATE_KEY_BYTES] = fs::read(path)?
        .try_into()
        .map_err(|_| invalid_key("attestation key length invalid"))?;
    Ok(SigningKey::from_bytes(&bytes))
}

fn validate_key_parent(path: &Path, expected_uid: u32) -> io::Result<()> {
    if !path.is_absolute() {
        return Err(invalid_key("attestation key path must be absolute"));
    }
    let parent = path
        .parent()
        .ok_or_else(|| invalid_key("attestation key parent missing"))?;
    let metadata = fs::symlink_metadata(parent)?;
    if !metadata.is_dir()
        || metadata.file_type().is_symlink()
        || metadata.uid() != expected_uid
        || metadata.permissions().mode() & 0o777 != 0o700
    {
        return Err(invalid_key("attestation key parent must be owner-only"));
    }
    Ok(())
}

fn invalid_key(detail: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::PermissionDenied, detail)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReceiptError {
    Missing,
    UnsupportedVersion,
    Malformed,
    SignatureInvalid,
}

impl ReceiptError {
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::Missing => "receipt_missing",
            Self::UnsupportedVersion => "receipt_version_unsupported",
            Self::Malformed => "receipt_malformed",
            Self::SignatureInvalid => "receipt_signature_invalid",
        }
    }
}

impl fmt::Display for ReceiptError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code())
    }
}

impl std::error::Error for ReceiptError {}

#[must_use]
pub fn key_id(key: &VerifyingKey) -> String {
    format!(
        "{KEY_ID_PREFIX}{}",
        hex::encode(Sha256::digest(key.as_bytes()))
    )
}

fn hash_result_envelope<P: Serialize>(
    result_kind: u32,
    payload: P,
) -> Result<[u8; 32], ReceiptError> {
    ocservia_canonical_cbor::hash(
        RESULT_PAYLOAD_DOMAIN,
        &CborResultPayloadEnvelope {
            result_kind,
            payload,
        },
    )
    .map_err(|_| ReceiptError::Malformed)
}

/// Computes the canonical CBOR SHA-256 over a structured command result payload,
/// independent of Protobuf wire serialization.
///
/// # Errors
///
/// Returns [`ReceiptError::Malformed`] when the result kind is unsupported,
/// empty kinds carry unexpected bytes, or the Protobuf payload fails to decode.
pub fn result_payload_cbor_hash(
    kind: PrivilegedResultKind,
    raw: &[u8],
) -> Result<[u8; 32], ReceiptError> {
    let result_kind = u32::try_from(i32::from(kind)).map_err(|_| ReceiptError::Malformed)?;
    match kind {
        PrivilegedResultKind::Mutation | PrivilegedResultKind::Error => {
            if !raw.is_empty() {
                return Err(ReceiptError::Malformed);
            }
            hash_result_envelope(result_kind, CborEmptyResult {})
        }
        PrivilegedResultKind::ConfigPlan => {
            let msg = ConfigPlanResult::decode(raw).map_err(|_| ReceiptError::Malformed)?;
            hash_result_envelope(
                result_kind,
                CborConfigPlanResult {
                    candidate_hash: Bytes(&msg.candidate_hash),
                    diff_redacted: &msg.diff_redacted,
                    warnings: &msg.warnings,
                    current_unchanged: msg.current_unchanged,
                    staging_cleaned: msg.staging_cleaned,
                    current_hash: Bytes(&msg.current_hash),
                    materialized_hash: Bytes(&msg.materialized_hash),
                },
            )
        }
        PrivilegedResultKind::ConfigApply => {
            let msg = ConfigApplyResult::decode(raw).map_err(|_| ReceiptError::Malformed)?;
            hash_result_envelope(
                result_kind,
                CborConfigApplyResult {
                    candidate_hash: Bytes(&msg.candidate_hash),
                    previous_hash: Bytes(&msg.previous_hash),
                    observed_hash: Bytes(&msg.observed_hash),
                    applied_revision: msg.applied_revision,
                    healthy: msg.healthy,
                    rolled_back: msg.rolled_back,
                    failed_critical: msg.failed_critical,
                    failure_code: &msg.failure_code,
                },
            )
        }
        PrivilegedResultKind::CertificateCsr => {
            let msg = CertificateCsrResult::decode(raw).map_err(|_| ReceiptError::Malformed)?;
            hash_result_envelope(
                result_kind,
                CborCertificateCsrResult {
                    certificate_id: Bytes(&msg.certificate_id),
                    csr_der: Bytes(&msg.csr_der),
                    public_key_sha256: Bytes(&msg.public_key_sha256),
                },
            )
        }
        PrivilegedResultKind::CertificateP12 => {
            let msg =
                CertificateArtifactResult::decode(raw).map_err(|_| ReceiptError::Malformed)?;
            hash_result_envelope(
                result_kind,
                CborCertificateArtifactResult {
                    certificate_id: Bytes(&msg.certificate_id),
                    artifact_id: Bytes(&msg.artifact_id),
                    artifact_sha256: Bytes(&msg.artifact_sha256),
                    artifact_size: msg.artifact_size,
                },
            )
        }
        PrivilegedResultKind::CertificateRevoke => {
            let msg = CertificateRevokeResult::decode(raw).map_err(|_| ReceiptError::Malformed)?;
            hash_result_envelope(
                result_kind,
                CborCertificateRevokeResult {
                    certificate_id: Bytes(&msg.certificate_id),
                    key_removed: msg.key_removed,
                },
            )
        }
        PrivilegedResultKind::AgentUpgradeScheduled => {
            let msg =
                AgentUpgradeScheduledResult::decode(raw).map_err(|_| ReceiptError::Malformed)?;
            hash_result_envelope(
                result_kind,
                CborAgentUpgradeScheduledResult {
                    operation_id: Bytes(&msg.operation_id),
                    target_version: &msg.target_version,
                    package_sha256: Bytes(&msg.package_sha256),
                },
            )
        }
        PrivilegedResultKind::Unspecified => Err(ReceiptError::Malformed),
    }
}

/// Signs a validated canonical receipt with the matching node key.
///
/// # Errors
///
/// Returns an error for malformed receipt fields or a mismatched key ID.
pub fn sign_receipt(
    receipt: PrivdResultReceiptV1,
    key: &SigningKey,
) -> Result<PrivilegedResultProof, ReceiptError> {
    let expected = key_id(&key.verifying_key());
    sign_receipt_with_key_id(receipt, key, &expected)
}

/// Signs a validated canonical receipt using a caller-cached key ID computed
/// from `key.verifying_key()`.
///
/// # Errors
///
/// Returns an error for malformed receipt fields or a mismatched key ID.
pub fn sign_receipt_with_key_id(
    receipt: PrivdResultReceiptV1,
    key: &SigningKey,
    expected_key_id: &str,
) -> Result<PrivilegedResultProof, ReceiptError> {
    let canonical = canonical_receipt_v1(&receipt)?;
    if receipt.privd_attestation_key_id != expected_key_id {
        return Err(ReceiptError::Malformed);
    }
    Ok(PrivilegedResultProof {
        version: 1,
        receipt_v1: Some(receipt),
        signature: key.sign(&canonical).to_bytes().to_vec(),
    })
}

/// Verifies a canonical receipt and returns its bound fields.
///
/// # Errors
///
/// Returns an error for a missing, malformed, unsupported, or invalidly signed receipt.
pub fn verify_receipt(
    proof: &PrivilegedResultProof,
    key: &VerifyingKey,
) -> Result<PrivdResultReceiptV1, ReceiptError> {
    if proof.version != 1 {
        return Err(ReceiptError::UnsupportedVersion);
    }
    let receipt = proof.receipt_v1.as_ref().ok_or(ReceiptError::Missing)?;
    if receipt.privd_attestation_key_id != key_id(key) {
        return Err(ReceiptError::Malformed);
    }
    let canonical = canonical_receipt_v1(receipt)?;
    let signature = Signature::from_slice(&proof.signature).map_err(|_| ReceiptError::Malformed)?;
    key.verify_strict(&canonical, &signature)
        .map_err(|_| ReceiptError::SignatureInvalid)?;
    Ok(receipt.clone())
}

/// Signs a root-owned durable upgrade outcome after validating every claim.
///
/// # Errors
///
/// Returns an error for malformed claims or a mismatched signing key ID.
pub fn sign_upgrade_result(
    proof: AgentUpgradeResultProof,
    key: &SigningKey,
) -> Result<AgentUpgradeResultProof, ReceiptError> {
    let expected = key_id(&key.verifying_key());
    sign_upgrade_result_with_key_id(proof, key, &expected)
}

/// Signs a root-owned durable upgrade outcome using a caller-cached key ID
/// computed from `key.verifying_key()`.
///
/// # Errors
///
/// Returns an error for malformed claims or a mismatched signing key ID.
pub fn sign_upgrade_result_with_key_id(
    mut proof: AgentUpgradeResultProof,
    key: &SigningKey,
    expected_key_id: &str,
) -> Result<AgentUpgradeResultProof, ReceiptError> {
    proof.signature.clear();
    let canonical = canonical_upgrade_result_v1(&proof)?;
    if proof.privd_attestation_key_id != expected_key_id {
        return Err(ReceiptError::Malformed);
    }
    proof.signature = key.sign(&canonical).to_bytes().to_vec();
    Ok(proof)
}

/// Verifies a root-owned durable upgrade outcome with its registered key.
///
/// # Errors
///
/// Returns an error for a missing, malformed, unsupported, or invalidly signed proof.
pub fn verify_upgrade_result(
    proof: &AgentUpgradeResultProof,
    key: &VerifyingKey,
) -> Result<AgentUpgradeResultProof, ReceiptError> {
    if proof.version != 1 {
        return Err(ReceiptError::UnsupportedVersion);
    }
    if proof.privd_attestation_key_id != key_id(key) {
        return Err(ReceiptError::Malformed);
    }
    let canonical = canonical_upgrade_result_v1(proof)?;
    let signature = Signature::from_slice(&proof.signature).map_err(|_| ReceiptError::Malformed)?;
    key.verify_strict(&canonical, &signature)
        .map_err(|_| ReceiptError::SignatureInvalid)?;
    Ok(proof.clone())
}

/// Encodes a V1 durable upgrade outcome with RFC 8949 Core Deterministic CBOR.
///
/// # Errors
///
/// Returns an error when any claim violates the V1 canonical contract.
pub fn canonical_upgrade_result_v1(
    proof: &AgentUpgradeResultProof,
) -> Result<Vec<u8>, ReceiptError> {
    validate_upgrade_result(proof)?;
    let state = u32::try_from(proof.state).map_err(|_| ReceiptError::Malformed)?;
    let encoded = ocservia_canonical_cbor::marshal(
        UPGRADE_RESULT_DOMAIN,
        &CborUpgradeResultProofV1 {
            version: proof.version,
            node_id: Bytes(&proof.node_id),
            privd_attestation_key_id: &proof.privd_attestation_key_id,
            operation_id: Bytes(&proof.operation_id),
            target_version: &proof.target_version,
            package_sha256: Bytes(&proof.package_sha256),
            state,
            completed_unix_ms: proof.completed_unix_ms,
            result_sha256: Bytes(&proof.result_sha256),
            attested_unix_ms: proof.attested_unix_ms,
        },
    )
    .map_err(|_| ReceiptError::Malformed)?;
    if encoded.len() > MAX_CANONICAL_BYTES {
        return Err(ReceiptError::Malformed);
    }
    Ok(encoded)
}

fn validate_upgrade_result(proof: &AgentUpgradeResultProof) -> Result<(), ReceiptError> {
    let state = AgentUpgradeOutcomeState::try_from(proof.state)
        .unwrap_or(AgentUpgradeOutcomeState::Unspecified);
    if proof.version != 1
        || !uuid_v7(&proof.node_id)
        || !uuid_v7(&proof.operation_id)
        || !valid_key_id(&proof.privd_attestation_key_id)
        || !ocservia_contracts::agent_upgrade::valid_target_version(&proof.target_version)
        || proof.package_sha256.len() != 32
        || !matches!(
            state,
            AgentUpgradeOutcomeState::Succeeded
                | AgentUpgradeOutcomeState::Failed
                | AgentUpgradeOutcomeState::RolledBack
        )
        || proof.completed_unix_ms == 0
        || proof.completed_unix_ms > i64::MAX as u64
        || proof.result_sha256.len() != 32
        || proof.attested_unix_ms == 0
        || proof.attested_unix_ms > i64::MAX as u64
        || proof.attested_unix_ms < proof.completed_unix_ms
    {
        return Err(ReceiptError::Malformed);
    }
    Ok(())
}

/// Encodes a V1 receipt with RFC 8949 Core Deterministic CBOR.
///
/// # Errors
///
/// Returns an error when any field violates the V1 canonical contract.
pub fn canonical_receipt_v1(receipt: &PrivdResultReceiptV1) -> Result<Vec<u8>, ReceiptError> {
    validate_receipt(receipt)?;
    let accepted = receipt
        .accepted_at
        .as_ref()
        .ok_or(ReceiptError::Malformed)?;
    let completed = receipt
        .completed_at
        .as_ref()
        .ok_or(ReceiptError::Malformed)?;
    let certificate = receipt
        .certificate
        .as_ref()
        .map(|binding| CborCertificateBinding {
            certificate_id: Bytes(&binding.certificate_id),
            csr_der_sha256: Bytes(&binding.csr_der_sha256),
            public_key_sha256: Bytes(&binding.public_key_sha256),
        });
    let encoded = ocservia_canonical_cbor::marshal(
        RECEIPT_DOMAIN,
        &CborReceiptV1 {
            receipt_version: receipt.receipt_version,
            node_id: Bytes(&receipt.node_id),
            privd_attestation_key_id: &receipt.privd_attestation_key_id,
            command_id: Bytes(&receipt.command_id),
            operation_id: Bytes(&receipt.operation_id),
            idempotency_key: Bytes(&receipt.idempotency_key),
            semantic_payload_hash_version: u32::try_from(receipt.semantic_payload_hash_version)
                .map_err(|_| ReceiptError::Malformed)?,
            semantic_payload_sha256: Bytes(&receipt.semantic_payload_sha256),
            command_kind: u32::try_from(receipt.command_kind)
                .map_err(|_| ReceiptError::Malformed)?,
            result_kind: u32::try_from(receipt.result_kind).map_err(|_| ReceiptError::Malformed)?,
            terminal_state: u32::try_from(receipt.terminal_state)
                .map_err(|_| ReceiptError::Malformed)?,
            result_cbor_sha256: Bytes(&receipt.result_cbor_sha256),
            error_code: &receipt.error_code,
            effect_sequence: receipt.effect_sequence,
            accepted_at_seconds: accepted.seconds,
            accepted_at_nanos: u32::try_from(accepted.nanos)
                .map_err(|_| ReceiptError::Malformed)?,
            completed_at_seconds: completed.seconds,
            completed_at_nanos: u32::try_from(completed.nanos)
                .map_err(|_| ReceiptError::Malformed)?,
            replayed: receipt.replayed,
            certificate,
        },
    )
    .map_err(|_| ReceiptError::Malformed)?;
    if encoded.len() > MAX_CANONICAL_BYTES {
        return Err(ReceiptError::Malformed);
    }
    Ok(encoded)
}

/// Encodes a V1 root key registration with RFC 8949 Core Deterministic CBOR.
///
/// # Errors
///
/// Returns an error when the registration is malformed or its key ID is inconsistent.
pub fn canonical_registration_v1(
    registration: &PrivdAttestationRegistrationV1,
) -> Result<Vec<u8>, ReceiptError> {
    if registration.version != 1
        || !uuid_v7(&registration.node_id)
        || registration.public_key.len() != 32
        || registration.controller_nonce.len() != 32
        || registration.credential_context_sha256.len() != 32
        || !valid_key_id(&registration.privd_attestation_key_id)
    {
        return Err(ReceiptError::Malformed);
    }
    let key: [u8; 32] = registration
        .public_key
        .as_slice()
        .try_into()
        .map_err(|_| ReceiptError::Malformed)?;
    let verifying = VerifyingKey::from_bytes(&key).map_err(|_| ReceiptError::Malformed)?;
    if registration.privd_attestation_key_id != key_id(&verifying) {
        return Err(ReceiptError::Malformed);
    }
    ocservia_canonical_cbor::marshal(
        REGISTRATION_DOMAIN,
        &CborRegistrationV1 {
            version: registration.version,
            node_id: Bytes(&registration.node_id),
            privd_attestation_key_id: &registration.privd_attestation_key_id,
            public_key: Bytes(&registration.public_key),
            controller_nonce: Bytes(&registration.controller_nonce),
            credential_context_sha256: Bytes(&registration.credential_context_sha256),
        },
    )
    .map_err(|_| ReceiptError::Malformed)
}

/// Signs a validated root key registration with the key being registered.
///
/// # Errors
///
/// Returns an error when the registration cannot be canonically encoded.
pub fn sign_registration(
    mut registration: PrivdAttestationRegistrationV1,
    key: &SigningKey,
) -> Result<PrivdAttestationRegistrationV1, ReceiptError> {
    registration.signature.clear();
    let canonical = canonical_registration_v1(&registration)?;
    registration.signature = key.sign(&canonical).to_bytes().to_vec();
    Ok(registration)
}

fn validate_receipt(receipt: &PrivdResultReceiptV1) -> Result<(), ReceiptError> {
    let semantic = SemanticPayloadHashVersion::try_from(receipt.semantic_payload_hash_version)
        .unwrap_or(SemanticPayloadHashVersion::Unspecified);
    let command = PrivilegedCommandKind::try_from(receipt.command_kind)
        .unwrap_or(PrivilegedCommandKind::Unspecified);
    let result = PrivilegedResultKind::try_from(receipt.result_kind)
        .unwrap_or(PrivilegedResultKind::Unspecified);
    let state = CommandResultState::try_from(receipt.terminal_state)
        .unwrap_or(CommandResultState::Unspecified);
    let accepted = receipt
        .accepted_at
        .as_ref()
        .ok_or(ReceiptError::Malformed)?;
    let completed = receipt
        .completed_at
        .as_ref()
        .ok_or(ReceiptError::Malformed)?;
    if receipt.receipt_version != 1
        || !uuid_v7(&receipt.node_id)
        || !uuid_v7(&receipt.command_id)
        || !uuid_v7(&receipt.operation_id)
        || !uuid_v7(&receipt.idempotency_key)
        || !valid_key_id(&receipt.privd_attestation_key_id)
        || semantic != SemanticPayloadHashVersion::V2
        || command == PrivilegedCommandKind::Unspecified
        || result == PrivilegedResultKind::Unspecified
        || !matches!(
            state,
            CommandResultState::Succeeded | CommandResultState::Failed
        )
        || receipt.semantic_payload_sha256.len() != 32
        || receipt.result_cbor_sha256.len() != 32
        || receipt.error_code.len() > MAX_ERROR_CODE_BYTES
        || receipt.effect_sequence == 0
        || receipt.effect_sequence > i64::MAX as u64
        || !valid_timestamp(accepted)
        || !valid_timestamp(completed)
        || (accepted.seconds, accepted.nanos) > (completed.seconds, completed.nanos)
        || !valid_certificate_binding(command, result, receipt.certificate.as_ref())
    {
        return Err(ReceiptError::Malformed);
    }
    Ok(())
}

fn valid_certificate_binding(
    command: PrivilegedCommandKind,
    result: PrivilegedResultKind,
    binding: Option<&PrivdCertificateReceiptBindingV1>,
) -> bool {
    let certificate_command = matches!(
        command,
        PrivilegedCommandKind::CertificateCsr
            | PrivilegedCommandKind::CertificateP12
            | PrivilegedCommandKind::CertificateRevoke
    );
    let Some(binding) = binding else {
        return !certificate_command;
    };
    if !certificate_command || !uuid_v7(&binding.certificate_id) {
        return false;
    }
    if result == PrivilegedResultKind::CertificateCsr {
        binding.csr_der_sha256.len() == 32 && binding.public_key_sha256.len() == 32
    } else {
        binding.csr_der_sha256.is_empty() && binding.public_key_sha256.is_empty()
    }
}

fn valid_timestamp(value: &prost_types::Timestamp) -> bool {
    value.seconds >= 0 && (0..1_000_000_000).contains(&value.nanos)
}

fn valid_key_id(value: &str) -> bool {
    value.len() == KEY_ID_PREFIX.len() + 64
        && value.starts_with(KEY_ID_PREFIX)
        && value[KEY_ID_PREFIX.len()..]
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
}

fn uuid_v7(value: &[u8]) -> bool {
    value.len() == 16 && value[6] >> 4 == 7 && value[8] >> 6 == 2
}

#[cfg(test)]
mod tests;
