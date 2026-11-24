//! End-to-end Controller command authorization independent of transportd.

#![forbid(unsafe_code)]

mod keyring;
mod semantic_hash;
#[cfg(test)]
#[allow(clippy::too_many_lines)]
mod tests;

use std::fmt;

use ed25519_dalek::VerifyingKey;
use ocservia_canonical_cbor::{Bytes, marshal};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, CommandEnvelope, SemanticPayloadHashVersion, SessionGrantV1, command_envelope,
};
use serde::Serialize;
use sha2::{Digest as _, Sha256};

pub use crate::keyring::{ControllerCommandKeyring, VerifiedCommand, load_verification_key};
pub use crate::semantic_hash::{semantic_payload_hash_v2, verify_semantic_payload_hash};

#[cfg(test)]
use std::os::unix::fs::PermissionsExt as _;
#[cfg(test)]
use std::path::Path;

#[cfg(test)]
use crate::keyring::verification_key_metadata_allowed;

/// The signed command revision with immutable execution deadlines.
pub const COMMAND_PROTOCOL_VERSION: &str = "1.2";
/// Legacy recovery grants may have lost their original execution deadline.
pub const LEGACY_COMMAND_PROTOCOL_VERSION: &str = "1.1";
const DOMAIN_SEPARATOR: &str = "ocservia/controller-command/cbor.v1\0";
const SESSION_GRANT_DOMAIN_SEPARATOR: &str = "ocservia/controller-session-grant/cbor.v1\0";
const ARTIFACT_GRANT_DOMAIN_SEPARATOR: &str = "ocservia/artifact-grant/cbor.v1\0";
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
    pub session_epoch: u64,
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
    pub session_epoch: u64,
    pub negotiated_capabilities: Vec<String>,
    pub expires_at_seconds: i64,
}

#[derive(Serialize)]
struct CborCommandClaimsV1<'a> {
    version: u32,
    key_id: &'a str,
    protocol_version: &'a str,
    command_id: Bytes<'a>,
    idempotency_key: Bytes<'a>,
    node_id: Bytes<'a>,
    operation_id: Bytes<'a>,
    actor_identity: &'a str,
    action: &'a str,
    required_capability: &'a str,
    expected_revision: u64,
    semantic_hash_version: u32,
    semantic_payload_sha256: Bytes<'a>,
    payload_kind: u32,
    delivery_mode: u32,
    issued_at_seconds: i64,
    issued_at_nanos: u32,
    expires_at_seconds: i64,
    expires_at_nanos: u32,
}

#[derive(Serialize)]
struct CborSessionGrantV1<'a> {
    version: u32,
    key_id: &'a str,
    protocol_major: u32,
    protocol_minor: u32,
    node_id: Bytes<'a>,
    endpoint_id: Bytes<'a>,
    authorization_revision: u64,
    session_epoch: u64,
    negotiated_capabilities: &'a [String],
    issued_at_seconds: i64,
    issued_at_nanos: u32,
    expires_at_seconds: i64,
    expires_at_nanos: u32,
}

#[derive(Serialize)]
struct CborArtifactGrantV1<'a> {
    version: u32,
    key_id: &'a str,
    node_id: Bytes<'a>,
    artifact_id: Bytes<'a>,
    certificate_id: Bytes<'a>,
    certificate_version: u64,
    operation_id: Bytes<'a>,
    authorized_subject: &'a str,
    purpose: &'a str,
    max_bytes: u64,
    issued_at_seconds: i64,
    issued_at_nanos: u32,
    expires_at_seconds: i64,
    expires_at_nanos: u32,
    grant_id: Bytes<'a>,
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
        version: grant.version,
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

/// Produces the exact RFC 8949 Core Deterministic CBOR bytes signed for `ArtifactGrantV1`.
///
/// # Errors
///
/// Returns an authorization error when a claim is outside the canonical V1 constraints.
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
        || claims.issued_at_nanos >= 1_000_000_000
        || claims.expires_at_nanos >= 1_000_000_000
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "artifact_grant_claims_invalid",
        ));
    }
    marshal(
        ARTIFACT_GRANT_DOMAIN_SEPARATOR,
        &CborArtifactGrantV1 {
            version: claims.version,
            key_id: &claims.key_id,
            node_id: Bytes(&claims.node_id),
            artifact_id: Bytes(&claims.artifact_id),
            certificate_id: Bytes(&claims.certificate_id),
            certificate_version: claims.certificate_version,
            operation_id: Bytes(&claims.operation_id),
            authorized_subject: &claims.authorized_subject,
            purpose: &claims.purpose,
            max_bytes: claims.max_bytes,
            issued_at_seconds: claims.issued_at_seconds,
            issued_at_nanos: claims.issued_at_nanos,
            expires_at_seconds: claims.expires_at_seconds,
            expires_at_nanos: claims.expires_at_nanos,
            grant_id: Bytes(&claims.grant_id),
        },
    )
    .map_err(|_| AuthorizationError::ClaimsInvalid("artifact_grant_claims_invalid"))
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
        version: grant.version,
        key_id: grant.key_id.clone(),
        protocol_major: grant.protocol_major,
        protocol_minor: grant.protocol_minor,
        node_id: fixed::<16>(&grant.node_id, "session_grant_node_invalid")?,
        endpoint_id: fixed::<32>(&grant.endpoint_id, "session_grant_endpoint_invalid")?,
        authorization_revision: grant.authorization_revision,
        session_epoch: grant.session_epoch,
        negotiated_capabilities: grant.negotiated_capabilities.clone(),
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos: timestamp_nanos(issued_at.nanos)?,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos: timestamp_nanos(expires_at.nanos)?,
    })
}

/// Produces the exact RFC 8949 Core Deterministic CBOR bytes signed for `SessionGrantV1`.
///
/// # Errors
///
/// Rejects invalid versions, revisions, epochs, timestamps, key IDs, or unsorted
/// and duplicate capabilities.
pub fn canonical_session_grant_v1(
    claims: &SessionGrantClaimsV1,
) -> Result<Vec<u8>, AuthorizationError> {
    if claims.version != 1
        || claims.key_id.is_empty()
        || claims.protocol_major == 0
        || claims.authorization_revision == 0
        || claims.session_epoch == 0
        || claims.negotiated_capabilities.len() > 128
        || claims.issued_at_nanos >= 1_000_000_000
        || claims.expires_at_nanos >= 1_000_000_000
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
    marshal(
        SESSION_GRANT_DOMAIN_SEPARATOR,
        &CborSessionGrantV1 {
            version: claims.version,
            key_id: &claims.key_id,
            protocol_major: claims.protocol_major,
            protocol_minor: claims.protocol_minor,
            node_id: Bytes(&claims.node_id),
            endpoint_id: Bytes(&claims.endpoint_id),
            authorization_revision: claims.authorization_revision,
            session_epoch: claims.session_epoch,
            negotiated_capabilities: &claims.negotiated_capabilities,
            issued_at_seconds: claims.issued_at_seconds,
            issued_at_nanos: claims.issued_at_nanos,
            expires_at_seconds: claims.expires_at_seconds,
            expires_at_nanos: claims.expires_at_nanos,
        },
    )
    .map_err(|_| AuthorizationError::ClaimsInvalid("session_grant_claims_invalid"))
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
pub fn claims_from_envelope_v1(
    envelope: &CommandEnvelope,
) -> Result<CommandAuthorizationV1, AuthorizationError> {
    let proof = envelope
        .authorization
        .as_ref()
        .ok_or(AuthorizationError::Missing)?;
    if proof.version != 1 {
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
    if SemanticPayloadHashVersion::try_from(envelope.semantic_payload_hash_version)
        .unwrap_or(SemanticPayloadHashVersion::Unspecified)
        != SemanticPayloadHashVersion::V2
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "semantic_hash_version_unsupported",
        ));
    }
    if envelope.delivery_mode == 0 {
        return Err(AuthorizationError::ClaimsInvalid("delivery_mode_invalid"));
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

/// Encodes exact `CommandAuthorizationV1` signing bytes using RFC 8949 Core Deterministic CBOR.
///
/// # Errors
///
/// Rejects incomplete claims and values outside the v1 bounds.
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
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_claims_invalid",
        ));
    }
    marshal(
        DOMAIN_SEPARATOR,
        &CborCommandClaimsV1 {
            version: claims.authorization_version,
            key_id: &claims.key_id,
            protocol_version: &claims.protocol_version,
            command_id: Bytes(&claims.command_id),
            idempotency_key: Bytes(&claims.idempotency_key),
            node_id: Bytes(&claims.node_id),
            operation_id: Bytes(&claims.operation_id),
            actor_identity: &claims.actor_identity,
            action: &claims.action,
            required_capability: &claims.required_capability,
            expected_revision: claims.expected_revision,
            semantic_hash_version: claims.semantic_hash_version,
            semantic_payload_sha256: Bytes(&claims.semantic_payload_sha256),
            payload_kind: claims.payload_kind,
            delivery_mode: claims.delivery_mode,
            issued_at_seconds: claims.issued_at_seconds,
            issued_at_nanos: claims.issued_at_nanos,
            expires_at_seconds: claims.expires_at_seconds,
            expires_at_nanos: claims.expires_at_nanos,
        },
    )
    .map_err(|_| AuthorizationError::ClaimsInvalid("command_authorization_claims_invalid"))
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

pub(crate) fn fixed<const N: usize>(
    value: &[u8],
    code: &'static str,
) -> Result<[u8; N], AuthorizationError> {
    value
        .try_into()
        .map_err(|_| AuthorizationError::ClaimsInvalid(code))
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
