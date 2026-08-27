//! Canonical root-privilege result receipts independent of Protobuf encoding.

#![forbid(unsafe_code)]

use std::fmt;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Write as _};
use std::os::unix::fs::{MetadataExt as _, OpenOptionsExt as _, PermissionsExt as _};
use std::path::Path;

use ed25519_dalek::{Signature, Signer as _, SigningKey, VerifyingKey};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgradeOutcomeState, AgentUpgradeResultProof, CertificateCsr, CommandResultState,
    PrivdAttestationRegistrationV1, PrivdCertificateReceiptBindingV1, PrivdReceiptVersion,
    PrivdResultReceiptV1, PrivilegedCommandKind, PrivilegedResultKind, PrivilegedResultProof,
    SemanticPayloadHashVersion,
};
use sha2::{Digest as _, Sha256};

const RECEIPT_DOMAIN: &[u8] = b"ocservia/privd-result-receipt/v1\0";
const REGISTRATION_DOMAIN: &[u8] = b"ocservia/privd-attestation-registration/v1\0";
const UPGRADE_RESULT_DOMAIN: &[u8] = b"ocservia/privd-upgrade-result/v1\0";
const KEY_ID_PREFIX: &str = "ed25519-sha256:";
const MAX_CANONICAL_BYTES: usize = 2048;
const PRIVATE_KEY_BYTES: usize = 32;

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

/// Signs a validated canonical receipt with the matching node key.
///
/// # Errors
///
/// Returns an error for malformed receipt fields or a mismatched key ID.
pub fn sign_receipt(
    receipt: PrivdResultReceiptV1,
    key: &SigningKey,
) -> Result<PrivilegedResultProof, ReceiptError> {
    let canonical = canonical_receipt_v1(&receipt)?;
    if receipt.privd_attestation_key_id != key_id(&key.verifying_key()) {
        return Err(ReceiptError::Malformed);
    }
    Ok(PrivilegedResultProof {
        version: PrivdReceiptVersion::V1.into(),
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
    if PrivdReceiptVersion::try_from(proof.version).unwrap_or(PrivdReceiptVersion::Unspecified)
        != PrivdReceiptVersion::V1
    {
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
/// The signature excludes its own field and is independent of Protobuf
/// encoding, so an Agent can only relay the resulting evidence.
pub fn sign_upgrade_result(
    mut proof: AgentUpgradeResultProof,
    key: &SigningKey,
) -> Result<AgentUpgradeResultProof, ReceiptError> {
    proof.signature.clear();
    let canonical = canonical_upgrade_result_v1(&proof)?;
    if proof.privd_attestation_key_id != key_id(&key.verifying_key()) {
        return Err(ReceiptError::Malformed);
    }
    proof.signature = key.sign(&canonical).to_bytes().to_vec();
    Ok(proof)
}

/// Verifies a root-owned durable upgrade outcome with its registered key.
pub fn verify_upgrade_result(
    proof: &AgentUpgradeResultProof,
    key: &VerifyingKey,
) -> Result<AgentUpgradeResultProof, ReceiptError> {
    if PrivdReceiptVersion::try_from(proof.version).unwrap_or(PrivdReceiptVersion::Unspecified)
        != PrivdReceiptVersion::V1
    {
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

/// Encodes a V1 durable upgrade outcome independently of Protobuf field order.
pub fn canonical_upgrade_result_v1(
    proof: &AgentUpgradeResultProof,
) -> Result<Vec<u8>, ReceiptError> {
    validate_upgrade_result(proof)?;
    let mut encoded = Vec::with_capacity(256);
    encoded.extend_from_slice(UPGRADE_RESULT_DOMAIN);
    encoded.extend_from_slice(
        &u32::try_from(proof.version)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    append_bytes(&mut encoded, &proof.node_id)?;
    append_string(&mut encoded, &proof.privd_attestation_key_id)?;
    append_bytes(&mut encoded, &proof.operation_id)?;
    append_string(&mut encoded, &proof.target_version)?;
    append_bytes(&mut encoded, &proof.package_sha256)?;
    encoded.extend_from_slice(
        &u32::try_from(proof.state)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    encoded.extend_from_slice(&proof.completed_unix_ms.to_be_bytes());
    append_bytes(&mut encoded, &proof.result_sha256)?;
    if encoded.len() > MAX_CANONICAL_BYTES {
        return Err(ReceiptError::Malformed);
    }
    Ok(encoded)
}

fn validate_upgrade_result(proof: &AgentUpgradeResultProof) -> Result<(), ReceiptError> {
    let version =
        PrivdReceiptVersion::try_from(proof.version).unwrap_or(PrivdReceiptVersion::Unspecified);
    let state = AgentUpgradeOutcomeState::try_from(proof.state)
        .unwrap_or(AgentUpgradeOutcomeState::Unspecified);
    if version != PrivdReceiptVersion::V1
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
    {
        return Err(ReceiptError::Malformed);
    }
    Ok(())
}

/// Encodes a V1 receipt independently of Protobuf field order and unknown fields.
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
    let mut encoded = Vec::with_capacity(512);
    encoded.extend_from_slice(RECEIPT_DOMAIN);
    encoded.extend_from_slice(
        &u32::try_from(receipt.receipt_version)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    append_bytes(&mut encoded, &receipt.node_id)?;
    append_string(&mut encoded, &receipt.privd_attestation_key_id)?;
    append_bytes(&mut encoded, &receipt.command_id)?;
    append_bytes(&mut encoded, &receipt.operation_id)?;
    append_bytes(&mut encoded, &receipt.idempotency_key)?;
    encoded.extend_from_slice(
        &u32::try_from(receipt.semantic_payload_hash_version)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    append_bytes(&mut encoded, &receipt.semantic_payload_sha256)?;
    encoded.extend_from_slice(
        &u32::try_from(receipt.command_kind)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    encoded.extend_from_slice(
        &u32::try_from(receipt.result_kind)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    encoded.extend_from_slice(
        &u32::try_from(receipt.terminal_state)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    append_bytes(&mut encoded, &receipt.result_bytes_sha256)?;
    append_bytes(&mut encoded, &receipt.error_code_sha256)?;
    append_bytes(&mut encoded, &receipt.effect_record_id)?;
    encoded.extend_from_slice(&receipt.effect_sequence.to_be_bytes());
    append_timestamp(&mut encoded, accepted)?;
    append_timestamp(&mut encoded, completed)?;
    encoded.push(u8::from(receipt.replayed));
    append_certificate(&mut encoded, receipt.certificate.as_ref())?;
    if encoded.len() > MAX_CANONICAL_BYTES {
        return Err(ReceiptError::Malformed);
    }
    Ok(encoded)
}

/// Hashes the validated canonical V1 receipt.
///
/// # Errors
///
/// Returns an error when the receipt cannot be canonically encoded.
pub fn receipt_digest(receipt: &PrivdResultReceiptV1) -> Result<[u8; 32], ReceiptError> {
    Ok(Sha256::digest(canonical_receipt_v1(receipt)?).into())
}

/// Canonical requested certificate subject digest shared with Controller.
///
/// # Errors
///
/// Returns an error for an invalid certificate ID, subject, key size, or DNS name set.
pub fn requested_subject_digest(request: &CertificateCsr) -> Result<[u8; 32], ReceiptError> {
    if !uuid_v7(&request.certificate_id)
        || request.common_name.is_empty()
        || request.common_name.len() > 253
        || !(2048..=8192).contains(&request.key_bits)
        || request.dns_names.len() > 64
    {
        return Err(ReceiptError::Malformed);
    }
    let mut names = request.dns_names.clone();
    names.sort();
    if names.windows(2).any(|pair| pair[0] == pair[1]) {
        return Err(ReceiptError::Malformed);
    }
    let mut encoded = b"ocservia/certificate-requested-subject/v1\0".to_vec();
    append_bytes(&mut encoded, &request.certificate_id)?;
    append_string(&mut encoded, &request.common_name)?;
    encoded.extend_from_slice(&request.key_bits.to_be_bytes());
    encoded.extend_from_slice(
        &u32::try_from(names.len())
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    for name in names {
        if name.is_empty() || name.len() > 253 || name != name.to_ascii_lowercase() {
            return Err(ReceiptError::Malformed);
        }
        append_string(&mut encoded, &name)?;
    }
    Ok(Sha256::digest(encoded).into())
}

/// Encodes a V1 root key registration independently of Protobuf encoding.
///
/// # Errors
///
/// Returns an error when the registration is malformed or its key ID is inconsistent.
pub fn canonical_registration_v1(
    registration: &PrivdAttestationRegistrationV1,
) -> Result<Vec<u8>, ReceiptError> {
    if PrivdReceiptVersion::try_from(registration.version)
        .unwrap_or(PrivdReceiptVersion::Unspecified)
        != PrivdReceiptVersion::V1
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
    let mut encoded = Vec::with_capacity(256);
    encoded.extend_from_slice(REGISTRATION_DOMAIN);
    encoded.extend_from_slice(
        &u32::try_from(registration.version)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    append_bytes(&mut encoded, &registration.node_id)?;
    append_string(&mut encoded, &registration.privd_attestation_key_id)?;
    append_bytes(&mut encoded, &registration.public_key)?;
    append_bytes(&mut encoded, &registration.controller_nonce)?;
    append_bytes(&mut encoded, &registration.credential_context_sha256)?;
    Ok(encoded)
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
    let version = PrivdReceiptVersion::try_from(receipt.receipt_version)
        .unwrap_or(PrivdReceiptVersion::Unspecified);
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
    if version != PrivdReceiptVersion::V1
        || !uuid_v7(&receipt.node_id)
        || !uuid_v7(&receipt.command_id)
        || !uuid_v7(&receipt.operation_id)
        || !uuid_v7(&receipt.idempotency_key)
        || !valid_key_id(&receipt.privd_attestation_key_id)
        || !matches!(
            semantic,
            SemanticPayloadHashVersion::V1 | SemanticPayloadHashVersion::V2
        )
        || command == PrivilegedCommandKind::Unspecified
        || result == PrivilegedResultKind::Unspecified
        || !matches!(
            state,
            CommandResultState::Succeeded | CommandResultState::Failed
        )
        || receipt.semantic_payload_sha256.len() != 32
        || receipt.result_bytes_sha256.len() != 32
        || receipt.error_code_sha256.len() != 32
        || !(16..=32).contains(&receipt.effect_record_id.len())
        || receipt.effect_sequence == 0
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
    if !certificate_command
        || !uuid_v7(&binding.certificate_id)
        || binding.root_effect_record_id.len() < 16
        || binding.root_effect_record_id.len() > 32
    {
        return false;
    }
    if result == PrivilegedResultKind::CertificateCsr {
        binding.csr_der_sha256.len() == 32
            && binding.public_key_sha256.len() == 32
            && binding.requested_subject_sha256.len() == 32
    } else {
        binding.csr_der_sha256.is_empty()
            && binding.public_key_sha256.is_empty()
            && binding.requested_subject_sha256.is_empty()
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

fn append_string(output: &mut Vec<u8>, value: &str) -> Result<(), ReceiptError> {
    if value.is_empty() || value.len() > 128 || !value.is_ascii() {
        return Err(ReceiptError::Malformed);
    }
    append_bytes(output, value.as_bytes())
}

fn append_bytes(output: &mut Vec<u8>, value: &[u8]) -> Result<(), ReceiptError> {
    let length = u32::try_from(value.len()).map_err(|_| ReceiptError::Malformed)?;
    output.extend_from_slice(&length.to_be_bytes());
    output.extend_from_slice(value);
    Ok(())
}

fn append_timestamp(
    output: &mut Vec<u8>,
    value: &prost_types::Timestamp,
) -> Result<(), ReceiptError> {
    if !valid_timestamp(value) {
        return Err(ReceiptError::Malformed);
    }
    output.extend_from_slice(&value.seconds.to_be_bytes());
    output.extend_from_slice(
        &u32::try_from(value.nanos)
            .map_err(|_| ReceiptError::Malformed)?
            .to_be_bytes(),
    );
    Ok(())
}

fn append_certificate(
    output: &mut Vec<u8>,
    value: Option<&PrivdCertificateReceiptBindingV1>,
) -> Result<(), ReceiptError> {
    let Some(value) = value else {
        output.push(0);
        return Ok(());
    };
    output.push(1);
    append_bytes(output, &value.certificate_id)?;
    append_bytes(output, &value.csr_der_sha256)?;
    append_bytes(output, &value.public_key_sha256)?;
    append_bytes(output, &value.requested_subject_sha256)?;
    append_bytes(output, &value.root_effect_record_id)
}

#[cfg(test)]
mod tests {
    use super::*;
    use prost_types::Timestamp;
    use std::os::unix::fs::symlink;

    fn fixture() -> (PrivdResultReceiptV1, SigningKey) {
        let key = SigningKey::from_bytes(&[7; 32]);
        let id = hex::decode("018f2a3b4c5d70008000000000000001").expect("id");
        let effect = hex::decode("018f2a3b4c5d70008000000000000005").expect("effect");
        (
            PrivdResultReceiptV1 {
                receipt_version: PrivdReceiptVersion::V1.into(),
                node_id: id.clone(),
                privd_attestation_key_id: key_id(&key.verifying_key()),
                command_id: hex::decode("018f2a3b4c5d70008000000000000002").expect("command"),
                operation_id: hex::decode("018f2a3b4c5d70008000000000000003").expect("operation"),
                idempotency_key: hex::decode("018f2a3b4c5d70008000000000000004")
                    .expect("idempotency"),
                semantic_payload_hash_version: SemanticPayloadHashVersion::V2.into(),
                semantic_payload_sha256: vec![0x11; 32],
                command_kind: PrivilegedCommandKind::CertificateCsr.into(),
                result_kind: PrivilegedResultKind::CertificateCsr.into(),
                terminal_state: CommandResultState::Succeeded.into(),
                result_bytes_sha256: vec![0x22; 32],
                error_code_sha256: Sha256::digest([]).to_vec(),
                effect_record_id: effect.clone(),
                effect_sequence: 9,
                accepted_at: Some(Timestamp {
                    seconds: 1_700_000_000,
                    nanos: 123,
                }),
                completed_at: Some(Timestamp {
                    seconds: 1_700_000_001,
                    nanos: 456,
                }),
                replayed: false,
                certificate: Some(PrivdCertificateReceiptBindingV1 {
                    certificate_id: id,
                    csr_der_sha256: vec![0x33; 32],
                    public_key_sha256: vec![0x44; 32],
                    requested_subject_sha256: vec![0x55; 32],
                    root_effect_record_id: effect,
                }),
            },
            key,
        )
    }

    #[test]
    fn receipt_signature_binds_every_security_identity() {
        let (receipt, key) = fixture();
        let canonical = canonical_receipt_v1(&receipt).expect("canonical");
        assert_eq!(
            hex::encode(canonical),
            "6f637365727669612f70726976642d726573756c742d726563656970742f7631000000000100000010018f2a3b4c5d700080000000000000010000004f656432353531392d7368613235363a6665383132633132663361623463653661633564623639616333353266393036636231623131656634336662333365323532656637666635353232363338383900000010018f2a3b4c5d7000800000000000000200000010018f2a3b4c5d7000800000000000000300000010018f2a3b4c5d70008000000000000004000000020000002011111111111111111111111111111111111111111111111111111111111111110000000c000000040000000100000020222222222222222222222222222222222222222222222222222222222222222200000020e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85500000010018f2a3b4c5d700080000000000000050000000000000009000000006553f1000000007b000000006553f101000001c8000100000010018f2a3b4c5d7000800000000000000100000020333333333333333333333333333333333333333333333333333333333333333300000020444444444444444444444444444444444444444444444444444444444444444400000020555555555555555555555555555555555555555555555555555555555555555500000010018f2a3b4c5d70008000000000000005"
        );
        let proof = sign_receipt(receipt, &key).expect("sign");
        verify_receipt(&proof, &key.verifying_key()).expect("verify");
        for mutate in [
            |value: &mut PrivdResultReceiptV1| value.node_id[15] ^= 1,
            |value: &mut PrivdResultReceiptV1| value.command_id[15] ^= 1,
            |value: &mut PrivdResultReceiptV1| value.operation_id[15] ^= 1,
            |value: &mut PrivdResultReceiptV1| value.idempotency_key[15] ^= 1,
            |value: &mut PrivdResultReceiptV1| value.semantic_payload_sha256[0] ^= 1,
            |value: &mut PrivdResultReceiptV1| value.result_bytes_sha256[0] ^= 1,
            |value: &mut PrivdResultReceiptV1| value.effect_record_id[15] ^= 1,
        ] {
            let mut tampered = proof.clone();
            mutate(tampered.receipt_v1.as_mut().expect("receipt"));
            assert_eq!(
                verify_receipt(&tampered, &key.verifying_key()),
                Err(ReceiptError::SignatureInvalid)
            );
        }
    }

    #[test]
    fn upgrade_result_signature_binds_durable_identity() {
        let key = SigningKey::from_bytes(&[9; 32]);
        let proof = AgentUpgradeResultProof {
            version: PrivdReceiptVersion::V1.into(),
            node_id: hex::decode("018f2a3b4c5d70008000000000000001").expect("node"),
            privd_attestation_key_id: key_id(&key.verifying_key()),
            operation_id: hex::decode("018f2a3b4c5d70008000000000000002").expect("operation"),
            target_version: "1.2.3".to_owned(),
            package_sha256: vec![0x11; 32],
            state: AgentUpgradeOutcomeState::Succeeded.into(),
            completed_unix_ms: 1_700_000_000_000,
            result_sha256: vec![0x22; 32],
            signature: Vec::new(),
        };
        let signed = sign_upgrade_result(proof, &key).expect("sign");
        verify_upgrade_result(&signed, &key.verifying_key()).expect("verify");

        let mut tampered = signed.clone();
        tampered.package_sha256[0] ^= 1;
        assert_eq!(
            verify_upgrade_result(&tampered, &key.verifying_key()),
            Err(ReceiptError::SignatureInvalid)
        );
    }

    #[test]
    fn unknown_version_and_malformed_signature_fail_closed() {
        let (receipt, key) = fixture();
        let mut proof = sign_receipt(receipt, &key).expect("sign");
        proof.version = 77;
        assert_eq!(
            verify_receipt(&proof, &key.verifying_key()),
            Err(ReceiptError::UnsupportedVersion)
        );
        proof.version = PrivdReceiptVersion::V1.into();
        proof.signature.push(0);
        assert_eq!(
            verify_receipt(&proof, &key.verifying_key()),
            Err(ReceiptError::Malformed)
        );
    }

    #[test]
    fn key_creation_is_atomic_owner_only_and_symlinks_fail_closed() {
        let directory = std::env::temp_dir().join(format!(
            "ocservia-privd-attestation-{}",
            hex::encode(rand::random::<[u8; 8]>())
        ));
        fs::create_dir(&directory).expect("create key directory");
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))
            .expect("set directory mode");
        let uid = rustix::process::geteuid().as_raw();
        let path = directory.join("attestation.key");
        let first = load_or_create_signing_key(&path, uid).expect("create key");
        let second = load_or_create_signing_key(&path, uid).expect("reload key");
        assert_eq!(first.to_bytes(), second.to_bytes());
        let metadata = fs::symlink_metadata(&path).expect("metadata");
        assert_eq!(metadata.permissions().mode() & 0o777, 0o600);
        assert_eq!(metadata.len(), 32);

        let target = directory.join("target.key");
        fs::rename(&path, &target).expect("move key");
        symlink(&target, &path).expect("create symlink");
        assert_eq!(
            load_or_create_signing_key(&path, uid)
                .expect_err("symlink must fail")
                .kind(),
            io::ErrorKind::PermissionDenied
        );
        fs::remove_dir_all(directory).expect("remove key fixture");
    }
}
