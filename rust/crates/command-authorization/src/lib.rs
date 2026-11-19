//! End-to-end Controller command authorization independent of transportd.

#![forbid(unsafe_code)]

mod fence;
mod keyring;
mod semantic_hash;
#[cfg(test)]
#[allow(clippy::too_many_lines)]
mod tests;

use std::fmt;

use ed25519_dalek::VerifyingKey;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, CommandAuthorizationVersion, CommandEnvelope, SemanticPayloadHashVersion,
    SessionGrantV1, command_envelope,
};
use sha2::{Digest as _, Sha256};

pub use crate::fence::{
    ConnectionFenceClaimsV2, FENCE_SIGNATURE_VERSION_ED25519_V1, FenceBindingClaimsV2,
    STATE_UPDATE_OPERATION_DOMAIN, VerifiedConnectionFenceV2, canonical_connection_fence_v2,
    canonical_fence_binding_v2, connection_fence_claims_v2, fence_binding_claims_v2,
    state_update_operation_id,
};
pub use crate::keyring::{ControllerCommandKeyring, load_verification_key};
pub use crate::semantic_hash::{
    semantic_payload_hash_v1, semantic_payload_hash_v2, verify_semantic_payload_hash,
};

#[cfg(test)]
use std::os::unix::fs::PermissionsExt as _;
#[cfg(test)]
use std::path::Path;

#[cfg(test)]
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantVersion, ConnectionFenceV2, FenceBindingV2, FenceOperationKind,
    FenceSignatureVersion, SessionGrantVersion,
};

#[cfg(test)]
use crate::keyring::verification_key_metadata_allowed;

/// The signed command revision with immutable execution deadlines.
pub const COMMAND_PROTOCOL_VERSION: &str = "1.2";
/// Legacy recovery grants may have lost their original execution deadline.
pub const LEGACY_COMMAND_PROTOCOL_VERSION: &str = "1.1";
const DOMAIN_SEPARATOR: &[u8] = b"ocservia/controller-command/v1\0";
const SESSION_GRANT_DOMAIN_SEPARATOR: &[u8] = b"ocservia/controller-session-grant/v1\0";
const ARTIFACT_GRANT_DOMAIN_SEPARATOR: &[u8] = b"ocservia/artifact-grant/v1\0";
pub(crate) const CONNECTION_FENCE_V2_DOMAIN_SEPARATOR: &[u8] = b"ocservia/connection-fence/v2\0";
pub(crate) const FENCE_BINDING_V2_DOMAIN_SEPARATOR: &[u8] = b"ocservia/fence-binding/v2\0";
const KEY_ID_PREFIX: &str = "ed25519-sha256:";
pub(crate) const MAX_FUTURE_SKEW_SECONDS: i64 = 300;

/// Independent, non-Protobuf `CommandAuthorizationV1` claims.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommandAuthorizationV1 {
    pub authorization_version: u32,
    pub key_id: String,
    pub protocol_version: String,
    pub command_id: [u8; 16],
    pub idempotency_key: [u8; 16],
    pub node_id: [u8; 16],
    pub operation_id: [u8; 16],
    pub actor_identity: String,
    pub action: String,
    pub required_capability: String,
    pub approval_id: Option<[u8; 16]>,
    pub approval_request_sha256: Option<[u8; 32]>,
    pub expected_revision: u64,
    pub semantic_hash_version: u32,
    pub semantic_payload_sha256: [u8; 32],
    pub payload_kind: u32,
    pub delivery_mode: u32,
    pub issued_at_seconds: i64,
    pub issued_at_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

/// Independent, non-Protobuf canonical claims for one Agent session.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SessionGrantClaimsV1 {
    pub version: u32,
    pub key_id: String,
    pub protocol_major: u32,
    pub protocol_minor: u32,
    pub node_id: [u8; 16],
    pub endpoint_id: [u8; 32],
    pub authorization_revision: u64,
    pub negotiated_capabilities: Vec<String>,
    pub issued_at_seconds: i64,
    pub issued_at_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

/// Independent, non-Protobuf canonical claims for one bounded artifact fetch.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactGrantClaimsV1 {
    pub version: u32,
    pub key_id: String,
    pub node_id: [u8; 16],
    pub artifact_id: [u8; 16],
    pub certificate_id: [u8; 16],
    pub certificate_version: u64,
    pub operation_id: [u8; 16],
    pub authorized_subject: String,
    pub purpose: String,
    pub max_bytes: u64,
    pub issued_at_seconds: i64,
    pub issued_at_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
    pub grant_id: [u8; 16],
}

/// Verified Controller authority retained for exactly one connected session.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VerifiedSessionGrant {
    pub authorization_revision: u64,
    pub negotiated_capabilities: Vec<String>,
    pub expires_at_seconds: i64,
}

/// A fail-closed authorization validation error.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AuthorizationError {
    Missing,
    UnsupportedVersion,
    UnknownKey,
    SignatureMalformed,
    SignatureInvalid,
    ClaimsInvalid(&'static str),
    PayloadUnsupported,
}

impl AuthorizationError {
    /// Stable Agent rejection code.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::Missing => "command_authorization_missing",
            Self::UnsupportedVersion => "command_authorization_version_unsupported",
            Self::UnknownKey => "command_authorization_key_unknown",
            Self::SignatureMalformed => "command_authorization_signature_malformed",
            Self::SignatureInvalid => "command_authorization_signature_invalid",
            Self::ClaimsInvalid(code) => code,
            Self::PayloadUnsupported => "command_authorization_payload_unsupported",
        }
    }
}

impl fmt::Display for AuthorizationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            formatter,
            "Controller command authorization rejected: {}",
            self.code()
        )
    }
}

impl std::error::Error for AuthorizationError {}

/// Projects a Protobuf artifact grant into its independent claims model.
///
/// # Errors
///
/// Returns an authorization error when a required claim is missing or has an
/// invalid fixed-width or timestamp encoding.
pub fn artifact_grant_claims_v1(
    grant: &ArtifactGrantV1,
) -> Result<ArtifactGrantClaimsV1, AuthorizationError> {
    let issued_at = grant
        .issued_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "artifact_grant_issued_at_missing",
        ))?;
    let expires_at = grant
        .expires_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "artifact_grant_expires_at_missing",
        ))?;
    Ok(ArtifactGrantClaimsV1 {
        version: u32::try_from(grant.version)
            .map_err(|_| AuthorizationError::UnsupportedVersion)?,
        key_id: grant.key_id.clone(),
        node_id: fixed::<16>(&grant.node_id, "artifact_grant_node_invalid")?,
        artifact_id: fixed::<16>(&grant.artifact_id, "artifact_grant_artifact_invalid")?,
        certificate_id: fixed::<16>(&grant.certificate_id, "artifact_grant_certificate_invalid")?,
        certificate_version: grant.certificate_version,
        operation_id: fixed::<16>(&grant.operation_id, "artifact_grant_operation_invalid")?,
        authorized_subject: grant.authorized_subject.clone(),
        purpose: grant.purpose.clone(),
        max_bytes: grant.max_bytes,
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos: timestamp_nanos(issued_at.nanos)?,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos: timestamp_nanos(expires_at.nanos)?,
        grant_id: fixed::<16>(&grant.grant_id, "artifact_grant_id_invalid")?,
    })
}

/// Produces the exact domain-separated bytes signed for `ArtifactGrantV1`.
///
/// # Errors
///
/// Returns an authorization error when a claim is outside the canonical V1
/// constraints or a variable-width value exceeds the encoding limit.
pub fn canonical_artifact_grant_v1(
    claims: &ArtifactGrantClaimsV1,
) -> Result<Vec<u8>, AuthorizationError> {
    if claims.version != 1
        || claims.key_id.is_empty()
        || claims.key_id.len() > 128
        || claims.certificate_version == 0
        || claims.authorized_subject.is_empty()
        || claims.authorized_subject.len() > 256
        || claims.purpose != "certificate_p12"
        || claims.max_bytes == 0
        || claims.max_bytes > 64 * 1024 * 1024
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "artifact_grant_claims_invalid",
        ));
    }
    let mut encoded = Vec::with_capacity(512);
    encoded.extend_from_slice(ARTIFACT_GRANT_DOMAIN_SEPARATOR);
    encoded.extend_from_slice(&claims.version.to_be_bytes());
    append_string(&mut encoded, &claims.key_id)?;
    encoded.extend_from_slice(&claims.node_id);
    encoded.extend_from_slice(&claims.artifact_id);
    encoded.extend_from_slice(&claims.certificate_id);
    encoded.extend_from_slice(&claims.certificate_version.to_be_bytes());
    encoded.extend_from_slice(&claims.operation_id);
    append_string(&mut encoded, &claims.authorized_subject)?;
    append_string(&mut encoded, &claims.purpose)?;
    encoded.extend_from_slice(&claims.max_bytes.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.grant_id);
    Ok(encoded)
}

/// Projects a Protobuf carrier into the independent session grant claims.
///
/// # Errors
///
/// Rejects missing timestamps and malformed fixed-width identifiers.
pub fn session_grant_claims_v1(
    grant: &SessionGrantV1,
) -> Result<SessionGrantClaimsV1, AuthorizationError> {
    let issued_at = grant
        .issued_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "session_grant_issued_at_missing",
        ))?;
    let expires_at = grant
        .expires_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "session_grant_expires_at_missing",
        ))?;
    Ok(SessionGrantClaimsV1 {
        version: u32::try_from(grant.version)
            .map_err(|_| AuthorizationError::UnsupportedVersion)?,
        key_id: grant.key_id.clone(),
        protocol_major: grant.protocol_major,
        protocol_minor: grant.protocol_minor,
        node_id: fixed::<16>(&grant.node_id, "session_grant_node_invalid")?,
        endpoint_id: fixed::<32>(&grant.endpoint_id, "session_grant_endpoint_invalid")?,
        authorization_revision: grant.authorization_revision,
        negotiated_capabilities: grant.negotiated_capabilities.clone(),
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos: timestamp_nanos(issued_at.nanos)?,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos: timestamp_nanos(expires_at.nanos)?,
    })
}

/// Produces the exact bytes signed for `SessionGrantV1` without serializing Protobuf.
///
/// # Errors
///
/// Rejects invalid versions, revisions, timestamps, key IDs, or unsorted and
/// duplicate capabilities.
pub fn canonical_session_grant_v1(
    claims: &SessionGrantClaimsV1,
) -> Result<Vec<u8>, AuthorizationError> {
    if claims.version != 1
        || claims.key_id.is_empty()
        || claims.protocol_major == 0
        || claims.authorization_revision == 0
        || claims.negotiated_capabilities.len() > 128
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "session_grant_claims_invalid",
        ));
    }
    let mut previous: Option<&str> = None;
    for capability in &claims.negotiated_capabilities {
        if capability.is_empty()
            || capability.len() > 128
            || previous.is_some_and(|value| value >= capability.as_str())
        {
            return Err(AuthorizationError::ClaimsInvalid(
                "session_grant_capabilities_invalid",
            ));
        }
        previous = Some(capability);
    }
    let mut encoded = Vec::with_capacity(512);
    encoded.extend_from_slice(SESSION_GRANT_DOMAIN_SEPARATOR);
    encoded.extend_from_slice(&claims.version.to_be_bytes());
    append_string(&mut encoded, &claims.key_id)?;
    encoded.extend_from_slice(&claims.protocol_major.to_be_bytes());
    encoded.extend_from_slice(&claims.protocol_minor.to_be_bytes());
    encoded.extend_from_slice(&claims.node_id);
    encoded.extend_from_slice(&claims.endpoint_id);
    encoded.extend_from_slice(&claims.authorization_revision.to_be_bytes());
    encoded.extend_from_slice(
        &u32::try_from(claims.negotiated_capabilities.len())
            .map_err(|_| AuthorizationError::ClaimsInvalid("session_grant_capabilities_invalid"))?
            .to_be_bytes(),
    );
    for capability in &claims.negotiated_capabilities {
        append_string(&mut encoded, capability)?;
    }
    encoded.extend_from_slice(&claims.issued_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_nanos.to_be_bytes());
    Ok(encoded)
}

/// Returns the stable key ID carried in authorization proofs.
#[must_use]
pub fn verification_key_id(key: &VerifyingKey) -> String {
    let digest = Sha256::digest(key.as_bytes());
    format!("{KEY_ID_PREFIX}{}", hex::encode(digest))
}

/// Projects a command envelope into the independent v1 claims model.
///
/// # Errors
///
/// Rejects incomplete claims, wrong action/capability bindings, and unsupported payloads.
#[allow(clippy::too_many_lines)]
pub fn claims_from_envelope_v1(
    envelope: &CommandEnvelope,
) -> Result<CommandAuthorizationV1, AuthorizationError> {
    let proof = envelope
        .authorization
        .as_ref()
        .ok_or(AuthorizationError::Missing)?;
    if CommandAuthorizationVersion::try_from(proof.version)
        .unwrap_or(CommandAuthorizationVersion::Unspecified)
        != CommandAuthorizationVersion::V1
    {
        return Err(AuthorizationError::UnsupportedVersion);
    }
    if envelope.protocol_version != COMMAND_PROTOCOL_VERSION
        && envelope.protocol_version != LEGACY_COMMAND_PROTOCOL_VERSION
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_protocol_version_unsupported",
        ));
    }
    let (payload_kind, action, capability) = payload_authorization(envelope)?;
    if envelope.action != action {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_action_mismatch",
        ));
    }
    if envelope.required_capability != capability {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_capability_mismatch",
        ));
    }
    if envelope.actor_id.trim().is_empty()
        || envelope.actor_id.len() > 256
        || proof.key_id.is_empty()
        || proof.key_id.len() > 128
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_identity_invalid",
        ));
    }
    let issued_at = envelope
        .issued_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "command_authorization_issued_at_missing",
        ))?;
    let expires_at = envelope
        .expires_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "command_authorization_expires_at_missing",
        ))?;
    let issued_at_nanos = timestamp_nanos(issued_at.nanos)?;
    let expires_at_nanos = timestamp_nanos(expires_at.nanos)?;
    if expires_at.seconds < issued_at.seconds
        || (expires_at.seconds == issued_at.seconds && expires_at_nanos <= issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_time_order_invalid",
        ));
    }
    if !matches!(
        SemanticPayloadHashVersion::try_from(envelope.semantic_payload_hash_version)
            .unwrap_or(SemanticPayloadHashVersion::Unspecified),
        SemanticPayloadHashVersion::V1 | SemanticPayloadHashVersion::V2
    ) {
        return Err(AuthorizationError::ClaimsInvalid(
            "semantic_hash_version_unsupported",
        ));
    }
    if envelope.delivery_mode == 0 {
        return Err(AuthorizationError::ClaimsInvalid("delivery_mode_invalid"));
    }
    let approval_id = optional_fixed::<16>(&envelope.approval_id, "approval_id_invalid")?;
    let approval_request_sha256 = optional_fixed::<32>(
        &envelope.approval_request_sha256,
        "approval_request_hash_invalid",
    )?;
    if approval_request_sha256.is_some() && approval_id.is_none() {
        return Err(AuthorizationError::ClaimsInvalid(
            "approval_request_hash_without_id",
        ));
    }
    Ok(CommandAuthorizationV1 {
        authorization_version: 1,
        key_id: proof.key_id.clone(),
        protocol_version: envelope.protocol_version.clone(),
        command_id: fixed(&envelope.command_id, "command_id_invalid")?,
        idempotency_key: fixed(&envelope.idempotency_key, "idempotency_key_invalid")?,
        node_id: fixed(&envelope.node_id, "node_id_invalid")?,
        operation_id: fixed(&envelope.operation_id, "operation_id_invalid")?,
        actor_identity: envelope.actor_id.clone(),
        action: envelope.action.clone(),
        required_capability: envelope.required_capability.clone(),
        approval_id,
        approval_request_sha256,
        expected_revision: envelope.expected_revision,
        semantic_hash_version: u32::try_from(envelope.semantic_payload_hash_version)
            .map_err(|_| AuthorizationError::ClaimsInvalid("semantic_hash_version_invalid"))?,
        semantic_payload_sha256: fixed(
            &envelope.semantic_payload_sha256,
            "semantic_payload_hash_missing",
        )?,
        payload_kind,
        delivery_mode: u32::try_from(envelope.delivery_mode)
            .map_err(|_| AuthorizationError::ClaimsInvalid("delivery_mode_invalid"))?,
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos,
    })
}

/// Encodes exact `CommandAuthorizationV1` signing bytes without Protobuf serialization.
///
/// # Errors
///
/// Rejects incomplete claims and values that cannot be represented by the frozen layout.
pub fn canonical_v1(claims: &CommandAuthorizationV1) -> Result<Vec<u8>, AuthorizationError> {
    if claims.authorization_version != 1
        || claims.key_id.is_empty()
        || claims.protocol_version.is_empty()
        || claims.actor_identity.is_empty()
        || claims.action.is_empty()
        || claims.required_capability.is_empty()
        || claims.semantic_hash_version == 0
        || claims.payload_kind == 0
        || claims.delivery_mode == 0
        || claims.issued_at_nanos >= 1_000_000_000
        || claims.expires_at_nanos >= 1_000_000_000
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
        || (claims.approval_request_sha256.is_some() && claims.approval_id.is_none())
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_claims_invalid",
        ));
    }
    let mut encoded = Vec::with_capacity(512);
    encoded.extend_from_slice(DOMAIN_SEPARATOR);
    encoded.extend_from_slice(&claims.authorization_version.to_be_bytes());
    append_string(&mut encoded, &claims.key_id)?;
    append_string(&mut encoded, &claims.protocol_version)?;
    encoded.extend_from_slice(&claims.command_id);
    encoded.extend_from_slice(&claims.idempotency_key);
    encoded.extend_from_slice(&claims.node_id);
    encoded.extend_from_slice(&claims.operation_id);
    append_string(&mut encoded, &claims.actor_identity)?;
    append_string(&mut encoded, &claims.action)?;
    append_string(&mut encoded, &claims.required_capability)?;
    append_optional(&mut encoded, claims.approval_id.as_ref());
    append_optional(&mut encoded, claims.approval_request_sha256.as_ref());
    encoded.extend_from_slice(&claims.expected_revision.to_be_bytes());
    encoded.extend_from_slice(&claims.semantic_hash_version.to_be_bytes());
    encoded.extend_from_slice(&claims.semantic_payload_sha256);
    encoded.extend_from_slice(&claims.payload_kind.to_be_bytes());
    encoded.extend_from_slice(&claims.delivery_mode.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_nanos.to_be_bytes());
    Ok(encoded)
}

fn payload_authorization(
    envelope: &CommandEnvelope,
) -> Result<(u32, &'static str, &'static str), AuthorizationError> {
    Ok(match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SessionDisconnect(_)) => {
            (100, "session.disconnect", "ocserv.session.disconnect")
        }
        Some(command_envelope::Payload::UserCreate(_)) => {
            (101, "user.create", "ocserv.users.write")
        }
        Some(command_envelope::Payload::UserDisable(_)) => {
            (102, "user.disable", "ocserv.users.write")
        }
        Some(command_envelope::Payload::ConfigPlan(_)) => {
            (103, "config.plan", "ocserv.config.plan")
        }
        Some(command_envelope::Payload::ConfigApply(_)) => {
            (104, "config.apply", "ocserv.config.apply")
        }
        Some(command_envelope::Payload::CompleteConfigPlan(_)) => (
            129,
            "config.plan",
            ocservia_contracts::config_profile::PLAN_CAPABILITY,
        ),
        Some(command_envelope::Payload::CompleteConfigApply(_)) => (
            130,
            "config.apply",
            ocservia_contracts::config_profile::APPLY_CAPABILITY,
        ),
        Some(command_envelope::Payload::ServiceReload(_)) => {
            (105, "service.reload", "ocserv.service.reload")
        }
        Some(command_envelope::Payload::SyntheticNoop(_)) => {
            (107, "operation.create", "synthetic.noop")
        }
        Some(command_envelope::Payload::SyntheticEcho(_)) => {
            (108, "operation.create", "synthetic.echo")
        }
        Some(command_envelope::Payload::SessionTerminate(_)) => {
            (112, "session.terminate", "ocserv.session.terminate")
        }
        Some(command_envelope::Payload::IpBanRemove(_)) => {
            (113, "ip_ban.remove", "ocserv.ip_ban.remove")
        }
        Some(command_envelope::Payload::UserPasswordRotate(_)) => {
            (114, "user.password.rotate", "ocserv.users.write")
        }
        Some(command_envelope::Payload::GroupApply(_)) => {
            (115, "group.apply", "ocserv.groups.write")
        }
        Some(command_envelope::Payload::UserEnable(_)) => {
            (116, "user.enable", "ocserv.users.write")
        }
        Some(command_envelope::Payload::CertificateCsr(_)) => {
            (117, "certificate.issue", "ocserv.certificate.issue")
        }
        Some(command_envelope::Payload::CertificateP12(_)) => (
            118,
            "certificate.private_key.export",
            "ocserv.certificate.issue",
        ),
        Some(command_envelope::Payload::CertificateRevoke(_)) => {
            (119, "certificate.revoke", "ocserv.certificate.revoke")
        }
        Some(command_envelope::Payload::AgentUpgrade(_)) => (
            128,
            "agent.upgrade",
            ocservia_contracts::agent_upgrade::supported_capability(&envelope.required_capability)
                .ok_or(AuthorizationError::PayloadUnsupported)?,
        ),
        _ => return Err(AuthorizationError::PayloadUnsupported),
    })
}

pub(crate) fn append_string(target: &mut Vec<u8>, value: &str) -> Result<(), AuthorizationError> {
    let length = u32::try_from(value.len())
        .map_err(|_| AuthorizationError::ClaimsInvalid("command_authorization_string_too_large"))?;
    target.extend_from_slice(&length.to_be_bytes());
    target.extend_from_slice(value.as_bytes());
    Ok(())
}

fn append_optional<const N: usize>(target: &mut Vec<u8>, value: Option<&[u8; N]>) {
    if let Some(value) = value {
        target.push(1);
        target.extend_from_slice(value);
    } else {
        target.push(0);
    }
}

pub(crate) fn fixed<const N: usize>(
    value: &[u8],
    code: &'static str,
) -> Result<[u8; N], AuthorizationError> {
    value
        .try_into()
        .map_err(|_| AuthorizationError::ClaimsInvalid(code))
}

fn optional_fixed<const N: usize>(
    value: &[u8],
    code: &'static str,
) -> Result<Option<[u8; N]>, AuthorizationError> {
    if value.is_empty() {
        return Ok(None);
    }
    fixed(value, code).map(Some)
}

pub(crate) fn timestamp_nanos(value: i32) -> Result<u32, AuthorizationError> {
    let value = u32::try_from(value)
        .map_err(|_| AuthorizationError::ClaimsInvalid("command_authorization_time_invalid"))?;
    if value >= 1_000_000_000 {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_time_invalid",
        ));
    }
    Ok(value)
}
