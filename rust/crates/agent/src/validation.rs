#[cfg(test)]
use ocservia_contracts::generated::ocserv::platform::agent::v1::ConfigApplyResult;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandDeliveryMode, CommandEnvelope, SealedSecretPurpose, SealedSecretV1,
    SemanticPayloadHashVersion, command_envelope,
};
use prost::Message as _;
use uuid::Uuid;

use crate::{CommandContext, CommandError, MAX_COMMAND_BYTES, MAX_FUTURE_SKEW_SECONDS};

/// Returns the `(resource_type, resource_key, desired_revision)` tracked in
/// local desired-state reconciliation for fixed resource mutations.
#[must_use]
pub fn desired_resource(envelope: &CommandEnvelope) -> Option<(&'static str, &str, u64)> {
    match envelope.payload.as_ref()? {
        command_envelope::Payload::UserCreate(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserDisable(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserEnable(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserPasswordRotate(value) => {
            Some(("user", &value.username, value.desired_revision))
        }
        command_envelope::Payload::GroupApply(value) => {
            Some(("group", &value.group_name, value.desired_revision))
        }
        command_envelope::Payload::CompleteConfigApply(value) => {
            Some(("config", "ocserv.conf", value.desired_revision))
        }
        _ => None,
    }
}

#[cfg(test)]
pub(crate) fn observed_result(envelope: &CommandEnvelope) -> Vec<u8> {
    match envelope.payload.as_ref() {
        Some(command_envelope::Payload::CompleteConfigApply(payload)) => ConfigApplyResult {
            candidate_hash: payload.candidate_hash.clone(),
            previous_hash: Vec::new(),
            observed_hash: payload.candidate_hash.clone(),
            applied_revision: payload.desired_revision,
            healthy: true,
            rolled_back: false,
            failed_critical: false,
            failure_code: String::new(),
        }
        .encode_to_vec(),
        _ => b"observed".to_vec(),
    }
}

pub(crate) struct ValidatedCommand {
    pub(crate) key: [u8; 16],
    pub(crate) command_id: [u8; 16],
    pub(crate) payload_hash: [u8; 32],
    pub(crate) hash_version: SemanticPayloadHashVersion,
    pub(crate) result: Vec<u8>,
    pub(crate) external: bool,
}

/// Computes the canonical CBOR semantic hash (v2) for a command envelope.
///
/// # Errors
///
/// Rejects malformed or unsupported typed command payloads.
pub fn semantic_payload_hash_v2(envelope: &CommandEnvelope) -> Result<[u8; 32], CommandError> {
    ocservia_command_authorization::semantic_payload_hash_v2(envelope)
        .map_err(|error| CommandError::Rejected(error.code()))
}

pub(crate) fn validate_command(
    envelope: &CommandEnvelope,
    context: &CommandContext,
) -> Result<ValidatedCommand, CommandError> {
    let encoded_len = envelope.encoded_len();
    if encoded_len == 0 || encoded_len > MAX_COMMAND_BYTES {
        return Err(CommandError::Rejected("command_size_invalid"));
    }
    if envelope.protocol_version != ocservia_command_authorization::COMMAND_PROTOCOL_VERSION {
        return Err(CommandError::Rejected("protocol_version_unsupported"));
    }
    let node_id = fixed::<16>(&envelope.node_id, "node_id_invalid")?;
    if node_id != context.node_id {
        return Err(CommandError::Rejected("node_id_mismatch"));
    }
    if context.cancelled {
        return Err(CommandError::Rejected("command_cancelled"));
    }
    let issued_at = envelope
        .issued_at
        .as_ref()
        .ok_or(CommandError::Rejected("issued_at_missing"))?
        .seconds;
    let expires_at = envelope
        .expires_at
        .as_ref()
        .ok_or(CommandError::Rejected("expires_at_missing"))?
        .seconds;
    if expires_at <= context.now_unix_seconds
        && envelope.delivery_mode != i32::from(CommandDeliveryMode::ReconcileOnly)
    {
        return Err(CommandError::Rejected("command_expired"));
    }
    if issued_at
        > context
            .now_unix_seconds
            .saturating_add(MAX_FUTURE_SKEW_SECONDS)
    {
        return Err(CommandError::Rejected("clock_skew"));
    }
    if context.authorization_revision == 0
        || envelope.expected_revision != context.authorization_revision
    {
        return Err(CommandError::Rejected("revision_mismatch"));
    }
    if context.session_expires_at_unix_seconds <= context.now_unix_seconds {
        return Err(CommandError::Rejected("session_grant_expired"));
    }
    let (capability, result, external) = validate_payload(envelope)?;
    if !context.capabilities.contains(capability) {
        return Err(CommandError::Rejected("capability_rejected"));
    }
    let key = fixed::<16>(&envelope.idempotency_key, "idempotency_key_invalid")?;
    let command_id = fixed::<16>(&envelope.command_id, "command_id_invalid")?;
    if Uuid::from_bytes(key).get_version_num() != 7 {
        return Err(CommandError::Rejected("idempotency_key_invalid"));
    }
    if Uuid::from_bytes(command_id).get_version_num() != 7 {
        return Err(CommandError::Rejected("command_id_invalid"));
    }
    let hash_version = SemanticPayloadHashVersion::try_from(envelope.semantic_payload_hash_version)
        .map_err(|_| CommandError::Rejected("semantic_hash_version_unsupported"))?;
    let payload_hash = ocservia_command_authorization::verify_semantic_payload_hash(envelope)
        .map_err(|error| CommandError::Rejected(error.code()))?;
    context
        .command_keys
        .verify(envelope)
        .map_err(|error| CommandError::Rejected(error.code()))?;
    Ok(ValidatedCommand {
        key,
        command_id,
        payload_hash,
        hash_version,
        result,
        external,
    })
}

#[allow(clippy::too_many_lines)]
pub(crate) fn validate_payload(
    envelope: &CommandEnvelope,
) -> Result<(&'static str, Vec<u8>, bool), CommandError> {
    Ok(match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(_)) => {
            ("synthetic.noop", b"noop:completed".to_vec(), false)
        }
        Some(command_envelope::Payload::SyntheticEcho(payload)) => {
            if payload.message.len() > 4096 {
                return Err(CommandError::Rejected("payload_size_invalid"));
            }
            ("synthetic.echo", payload.message.as_bytes().to_vec(), false)
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            validate_session_payload(&payload.session_id, &payload.boot_id)?;
            ("ocserv.session.disconnect", Vec::new(), true)
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            validate_session_payload(&payload.session_id, &payload.boot_id)?;
            ("ocserv.session.terminate", Vec::new(), true)
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            let canonical = payload
                .ip
                .parse::<std::net::IpAddr>()
                .map_err(|_| CommandError::Rejected("ip_invalid"))?
                .to_string();
            if canonical != payload.ip {
                return Err(CommandError::Rejected("ip_not_canonical"));
            }
            ("ocserv.ip_ban.remove", Vec::new(), true)
        }
        Some(command_envelope::Payload::ServiceReload(_)) => {
            ("ocserv.service.reload", Vec::new(), true)
        }
        Some(command_envelope::Payload::CompleteConfigPlan(_)) => (
            ocservia_contracts::config_profile::PLAN_CAPABILITY,
            Vec::new(),
            true,
        ),
        Some(command_envelope::Payload::CompleteConfigApply(_)) => (
            ocservia_contracts::config_profile::APPLY_CAPABILITY,
            Vec::new(),
            true,
        ),
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            if payload.common_name.len() > 253 {
                return Err(CommandError::Rejected("certificate_csr_invalid"));
            }
            ("ocserv.certificate.issue", Vec::new(), true)
        }
        Some(command_envelope::Payload::CertificateP12(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.artifact_id.len() != 16
                || payload.certificate_chain_pem.len() < 64
                || payload.certificate_chain_pem.len() > 256 * 1024
                || payload.certificate_version == 0
                || payload.artifact_expires_at.is_none()
            {
                return Err(CommandError::Rejected("certificate_p12_invalid"));
            }
            validate_typed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::CertificateP12Password,
            )?;
            ("ocserv.certificate.issue", Vec::new(), true)
        }
        Some(command_envelope::Payload::CertificateRevoke(_)) => {
            ("ocserv.certificate.revoke", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserCreate(payload)) => {
            validate_name(&payload.username)?;
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                payload.desired_revision,
            )?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserDisable(payload)) => {
            validate_name(&payload.username)?;
            validate_revision(payload.desired_revision)?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserEnable(payload)) => {
            validate_name(&payload.username)?;
            validate_revision(payload.desired_revision)?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            validate_name(&payload.username)?;
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                payload.desired_revision,
            )?;
            ("ocserv.users.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            validate_name(&payload.group_name)?;
            validate_revision(payload.desired_revision)?;
            if payload.members.len() > ocservia_agent_protocol::MAX_MANAGED_RESOURCES {
                return Err(CommandError::Rejected("group_members_invalid"));
            }
            let mut previous = None;
            for member in &payload.members {
                validate_name(member)?;
                if previous.is_some_and(|value: &String| value >= member) {
                    return Err(CommandError::Rejected("group_members_invalid"));
                }
                previous = Some(member);
            }
            ("ocserv.groups.write", Vec::new(), true)
        }
        Some(command_envelope::Payload::AgentUpgrade(payload)) => {
            validate_agent_upgrade(payload)?;
            (
                ocservia_contracts::agent_upgrade::supported_capability(
                    &envelope.required_capability,
                )
                .ok_or(CommandError::Rejected("capability_rejected"))?,
                Vec::new(),
                true,
            )
        }
        _ => return Err(CommandError::Rejected("capability_rejected")),
    })
}

/// Validates the typed upgrade release identity before any external effect.
/// The same checks are repeated independently by privd after its own
/// Controller-authorization verification.
fn validate_agent_upgrade(
    payload: &ocservia_contracts::generated::ocserv::platform::agent::v1::AgentUpgrade,
) -> Result<(), CommandError> {
    if !ocservia_contracts::agent_upgrade::valid_target_version(&payload.target_version) {
        return Err(CommandError::Rejected("agent_upgrade_version_invalid"));
    }
    if payload.package_sha256.len() != 32 {
        return Err(CommandError::Rejected("agent_upgrade_digest_invalid"));
    }
    if !ocservia_contracts::agent_upgrade::valid_architecture(&payload.architecture) {
        return Err(CommandError::Rejected("agent_upgrade_architecture_invalid"));
    }
    if ocservia_contracts::agent_upgrade::runtime_architecture()
        != Some(payload.architecture.as_str())
    {
        return Err(CommandError::Rejected(
            "agent_upgrade_architecture_mismatch",
        ));
    }
    Ok(())
}

fn validate_session_payload(session_id: &str, boot_id: &str) -> Result<(), CommandError> {
    let parsed = session_id
        .parse::<u64>()
        .map_err(|_| CommandError::Rejected("session_id_invalid"))?;
    if parsed == 0 || parsed.to_string() != session_id {
        return Err(CommandError::Rejected("session_id_invalid"));
    }
    if Uuid::parse_str(boot_id).is_err() {
        return Err(CommandError::Rejected("boot_id_invalid"));
    }
    Ok(())
}

fn validate_name(value: &str) -> Result<(), CommandError> {
    if !ocservia_contracts::unix::is_valid_resource_name(value) {
        return Err(CommandError::Rejected("name_invalid"));
    }
    Ok(())
}

fn validate_revision(value: u64) -> Result<(), CommandError> {
    if value == 0 {
        return Err(CommandError::Rejected("revision_invalid"));
    }
    Ok(())
}
fn validate_sealed_secret(
    secret: Option<&SealedSecretV1>,
    revision: u64,
) -> Result<(), CommandError> {
    validate_revision(revision)?;
    validate_typed_secret(secret, SealedSecretPurpose::UserPassword)
}

fn validate_typed_secret(
    secret: Option<&SealedSecretV1>,
    purpose: SealedSecretPurpose,
) -> Result<(), CommandError> {
    let secret = secret.ok_or(CommandError::Rejected("sealed_secret_invalid"))?;
    if secret.version != 1
        || SealedSecretPurpose::try_from(secret.purpose).ok() != Some(purpose)
        || secret.key_id.is_empty()
        || secret.key_id.len() > 128
        || secret.ciphertext.len() < 32
        || secret.ciphertext.len() > 16 * 1024
    {
        return Err(CommandError::Rejected("sealed_secret_invalid"));
    }
    Ok(())
}

fn fixed<const N: usize>(value: &[u8], code: &'static str) -> Result<[u8; N], CommandError> {
    value.try_into().map_err(|_| CommandError::Rejected(code))
}
