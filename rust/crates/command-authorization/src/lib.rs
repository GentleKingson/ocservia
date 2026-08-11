//! End-to-end Controller command authorization independent of transportd.

#![forbid(unsafe_code)]

use std::collections::HashMap;
use std::fmt;
use std::fs::File;
use std::io::{self, Read as _};
use std::os::unix::fs::{MetadataExt as _, PermissionsExt as _};
use std::path::{Component, Path};

use ed25519_dalek::pkcs8::DecodePublicKey as _;
use ed25519_dalek::{Signature, VerifyingKey};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, ArtifactGrantVersion, CommandAuthorizationVersion, CommandEnvelope,
    SealedSecretPurpose, SealedSecretV1, SealedSecretVersion, SemanticPayloadHashVersion,
    SessionGrantV1, SessionGrantVersion, command_envelope,
};
use rustix::fs::{Mode, OFlags};
use sha2::{Digest as _, Sha256};

/// The command protocol revision that requires authorization v1.
pub const COMMAND_PROTOCOL_VERSION: &str = "1.1";
const DOMAIN_SEPARATOR: &[u8] = b"ocservia/controller-command/v1\0";
const SESSION_GRANT_DOMAIN_SEPARATOR: &[u8] = b"ocservia/controller-session-grant/v1\0";
const ARTIFACT_GRANT_DOMAIN_SEPARATOR: &[u8] = b"ocservia/artifact-grant/v1\0";
const KEY_ID_PREFIX: &str = "ed25519-sha256:";
const MAX_FUTURE_SKEW_SECONDS: i64 = 300;

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

/// A bounded set of Controller public keys pinned by the Agent.
#[derive(Clone, Debug)]
pub struct ControllerCommandKeyring {
    keys: HashMap<String, VerifyingKey>,
}

impl ControllerCommandKeyring {
    /// Creates a keyring from one or more distinct pinned keys.
    ///
    /// # Errors
    ///
    /// Rejects an empty key list, more than eight rotation keys, or duplicate IDs.
    pub fn new(keys: impl IntoIterator<Item = VerifyingKey>) -> Result<Self, AuthorizationError> {
        let mut values = HashMap::new();
        for key in keys {
            let key_id = verification_key_id(&key);
            if values.insert(key_id, key).is_some() {
                return Err(AuthorizationError::ClaimsInvalid(
                    "command_authorization_key_duplicate",
                ));
            }
            if values.len() > 8 {
                return Err(AuthorizationError::ClaimsInvalid(
                    "command_authorization_key_count_invalid",
                ));
            }
        }
        if values.is_empty() {
            return Err(AuthorizationError::ClaimsInvalid(
                "command_authorization_key_missing",
            ));
        }
        Ok(Self { keys: values })
    }

    /// Verifies key selection, canonical claims, and the strict Ed25519 signature.
    ///
    /// # Errors
    ///
    /// Rejects missing/unknown proofs, malformed claims, and invalid signatures.
    pub fn verify(&self, envelope: &CommandEnvelope) -> Result<(), AuthorizationError> {
        self.verify_claims(envelope).map(|_| ())
    }

    fn verify_claims(
        &self,
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
        let key = self
            .keys
            .get(&proof.key_id)
            .ok_or(AuthorizationError::UnknownKey)?;
        let claims = claims_from_envelope_v1(envelope)?;
        let canonical = canonical_v1(&claims)?;
        let signature = Signature::from_slice(&proof.signature)
            .map_err(|_| AuthorizationError::SignatureMalformed)?;
        key.verify_strict(&canonical, &signature)
            .map_err(|_| AuthorizationError::SignatureInvalid)?;
        Ok(claims)
    }

    /// Independently verifies a command for one local node at the supplied
    /// wall-clock time, including its signed semantic payload identity.
    ///
    /// # Errors
    ///
    /// Rejects invalid signatures, another node's proof, expired or
    /// future-issued commands, zero authority revisions, and semantic hash
    /// mismatches.
    pub fn verify_command(
        &self,
        envelope: &CommandEnvelope,
        expected_node_id: &[u8; 16],
        now_unix_seconds: i64,
    ) -> Result<CommandAuthorizationV1, AuthorizationError> {
        let claims = self.verify_claims(envelope)?;
        if &claims.node_id != expected_node_id {
            return Err(AuthorizationError::ClaimsInvalid("node_id_mismatch"));
        }
        if claims.expected_revision == 0 {
            return Err(AuthorizationError::ClaimsInvalid("revision_invalid"));
        }
        if claims.expires_at_seconds <= now_unix_seconds {
            return Err(AuthorizationError::ClaimsInvalid("command_expired"));
        }
        if claims.issued_at_seconds > now_unix_seconds.saturating_add(MAX_FUTURE_SKEW_SECONDS) {
            return Err(AuthorizationError::ClaimsInvalid("clock_skew"));
        }
        verify_semantic_payload_hash(envelope)?;
        Ok(claims)
    }

    /// Verifies a Controller-signed session grant against the local node and
    /// pinned Iroh endpoint identities.
    ///
    /// # Errors
    ///
    /// Rejects unknown keys, invalid signatures, malformed or mismatched
    /// claims, future issuance, and expired grants.
    pub fn verify_session_grant(
        &self,
        grant: &SessionGrantV1,
        expected_node_id: &[u8; 16],
        expected_endpoint_id: &[u8; 32],
        now_unix_seconds: i64,
    ) -> Result<VerifiedSessionGrant, AuthorizationError> {
        let version = SessionGrantVersion::try_from(grant.version)
            .unwrap_or(SessionGrantVersion::Unspecified);
        if version != SessionGrantVersion::V1 {
            return Err(AuthorizationError::UnsupportedVersion);
        }
        let key = self
            .keys
            .get(&grant.key_id)
            .ok_or(AuthorizationError::UnknownKey)?;
        let claims = session_grant_claims_v1(grant)?;
        if &claims.node_id != expected_node_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "session_grant_node_mismatch",
            ));
        }
        if &claims.endpoint_id != expected_endpoint_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "session_grant_endpoint_mismatch",
            ));
        }
        if claims.expires_at_seconds <= now_unix_seconds {
            return Err(AuthorizationError::ClaimsInvalid("session_grant_expired"));
        }
        if claims.issued_at_seconds > now_unix_seconds.saturating_add(MAX_FUTURE_SKEW_SECONDS) {
            return Err(AuthorizationError::ClaimsInvalid(
                "session_grant_clock_skew",
            ));
        }
        let canonical = canonical_session_grant_v1(&claims)?;
        let signature = Signature::from_slice(&grant.signature)
            .map_err(|_| AuthorizationError::SignatureMalformed)?;
        key.verify_strict(&canonical, &signature)
            .map_err(|_| AuthorizationError::SignatureInvalid)?;
        Ok(VerifiedSessionGrant {
            authorization_revision: claims.authorization_revision,
            negotiated_capabilities: claims.negotiated_capabilities,
            expires_at_seconds: claims.expires_at_seconds,
        })
    }

    /// Verifies a Controller-signed artifact grant for the local node and the
    /// exact request carrier. transportd cannot mint or alter these claims.
    ///
    /// # Errors
    ///
    /// Returns an authorization error when the grant is malformed, expired,
    /// signed by an unknown key, has an invalid signature, or does not match
    /// the expected request claims.
    pub fn verify_artifact_grant(
        &self,
        grant: &ArtifactGrantV1,
        expected_node_id: &[u8; 16],
        expected_artifact_id: &[u8; 16],
        expected_purpose: &str,
        expected_max_bytes: u64,
        now_unix_seconds: i64,
    ) -> Result<ArtifactGrantClaimsV1, AuthorizationError> {
        self.verify_artifact_grant_inner(
            grant,
            expected_node_id,
            expected_artifact_id,
            expected_purpose,
            expected_max_bytes,
            now_unix_seconds,
            true,
        )
    }

    /// Verifies the signature and exact claims for a read-only consumed-state
    /// confirmation. Expiry is intentionally not enforced because this method
    /// cannot authorize a lease, read, deletion, or any other mutation.
    ///
    /// # Errors
    ///
    /// Returns an authorization error when the signature or exact request
    /// claims are invalid, even though expiry is not enforced.
    pub fn verify_artifact_grant_for_confirmation(
        &self,
        grant: &ArtifactGrantV1,
        expected_node_id: &[u8; 16],
        expected_artifact_id: &[u8; 16],
        expected_purpose: &str,
        expected_max_bytes: u64,
        now_unix_seconds: i64,
    ) -> Result<ArtifactGrantClaimsV1, AuthorizationError> {
        self.verify_artifact_grant_inner(
            grant,
            expected_node_id,
            expected_artifact_id,
            expected_purpose,
            expected_max_bytes,
            now_unix_seconds,
            false,
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn verify_artifact_grant_inner(
        &self,
        grant: &ArtifactGrantV1,
        expected_node_id: &[u8; 16],
        expected_artifact_id: &[u8; 16],
        expected_purpose: &str,
        expected_max_bytes: u64,
        now_unix_seconds: i64,
        require_unexpired: bool,
    ) -> Result<ArtifactGrantClaimsV1, AuthorizationError> {
        let version = ArtifactGrantVersion::try_from(grant.version)
            .unwrap_or(ArtifactGrantVersion::Unspecified);
        if version != ArtifactGrantVersion::V1 {
            return Err(AuthorizationError::UnsupportedVersion);
        }
        let key = self
            .keys
            .get(&grant.key_id)
            .ok_or(AuthorizationError::UnknownKey)?;
        let claims = artifact_grant_claims_v1(grant)?;
        if &claims.node_id != expected_node_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "artifact_grant_node_mismatch",
            ));
        }
        if &claims.artifact_id != expected_artifact_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "artifact_grant_artifact_mismatch",
            ));
        }
        if claims.purpose != expected_purpose || claims.max_bytes != expected_max_bytes {
            return Err(AuthorizationError::ClaimsInvalid(
                "artifact_grant_request_mismatch",
            ));
        }
        if require_unexpired && claims.expires_at_seconds <= now_unix_seconds {
            return Err(AuthorizationError::ClaimsInvalid("artifact_grant_expired"));
        }
        if claims.issued_at_seconds > now_unix_seconds.saturating_add(MAX_FUTURE_SKEW_SECONDS) {
            return Err(AuthorizationError::ClaimsInvalid(
                "artifact_grant_clock_skew",
            ));
        }
        let canonical = canonical_artifact_grant_v1(&claims)?;
        let signature = Signature::from_slice(&grant.signature)
            .map_err(|_| AuthorizationError::SignatureMalformed)?;
        key.verify_strict(&canonical, &signature)
            .map_err(|_| AuthorizationError::SignatureInvalid)?;
        Ok(claims)
    }
}

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
    if envelope.protocol_version != COMMAND_PROTOCOL_VERSION {
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

#[allow(clippy::too_many_lines)]
fn canonical_semantic_payload_hash(
    envelope: &CommandEnvelope,
    version: SemanticPayloadHashVersion,
) -> Result<[u8; 32], AuthorizationError> {
    let (payload_kind, canonical_payload) = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SyntheticNoop(_)) => (107_u32, Vec::new()),
        Some(command_envelope::Payload::SyntheticEcho(payload)) => {
            let mut out = Vec::with_capacity(4 + payload.message.len());
            append_semantic_bytes(&mut out, payload.message.as_bytes())?;
            (108_u32, out)
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => (
            100_u32,
            canonical_session_payload(&payload.session_id, &payload.boot_id)?,
        ),
        Some(command_envelope::Payload::ServiceReload(_)) => (105_u32, Vec::new()),
        Some(command_envelope::Payload::ConfigPlan(payload)) => {
            if payload.candidate_hash.len() != 32 {
                return Err(invalid_claim("candidate_hash_invalid"));
            }
            let mut out = Vec::with_capacity(40);
            out.extend_from_slice(&payload.candidate_hash);
            if version == SemanticPayloadHashVersion::V2 {
                out.extend_from_slice(&payload.expected_revision.to_be_bytes());
            }
            (103_u32, out)
        }
        Some(command_envelope::Payload::ConfigApply(payload)) => {
            if payload.candidate_hash.len() != 32
                || payload.expected_current_hash.len() != 32
                || payload.desired_revision == 0
            {
                return Err(invalid_claim("config_apply_invalid"));
            }
            let mut out = Vec::with_capacity(72);
            out.extend_from_slice(&payload.candidate_hash);
            out.extend_from_slice(&payload.expected_current_hash);
            out.extend_from_slice(&payload.desired_revision.to_be_bytes());
            (104_u32, out)
        }
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            if payload.certificate_id.len() != 16
                || payload.common_name.is_empty()
                || payload.dns_names.len() > 32
                || !matches!(payload.key_bits, 2048 | 3072 | 4096)
            {
                return Err(invalid_claim("certificate_csr_invalid"));
            }
            let values = std::iter::once(payload.common_name.as_str())
                .chain(payload.dns_names.iter().map(String::as_str))
                .collect::<Vec<_>>();
            (
                117_u32,
                canonical_strings_and_bytes(
                    &values,
                    &payload.certificate_id,
                    u64::from(payload.key_bits),
                )?,
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
                || !payload.sealed_password.is_empty()
                || !payload.secret_key_id.is_empty()
                || payload.certificate_version == 0
                || !(0..=999_999_999).contains(&expires.nanos)
            {
                return Err(invalid_claim("certificate_p12_invalid"));
            }
            let secret_bytes =
                canonical_sealed_secret(secret, SealedSecretPurpose::CertificateP12Password)?;
            let mut data = Vec::with_capacity(
                payload.certificate_id.len()
                    + payload.artifact_id.len()
                    + payload.certificate_chain_pem.len()
                    + secret_bytes.len()
                    + 12,
            );
            data.extend_from_slice(&payload.certificate_id);
            data.extend_from_slice(&payload.artifact_id);
            data.extend_from_slice(&payload.certificate_chain_pem);
            data.extend_from_slice(&secret_bytes);
            data.extend_from_slice(&expires.seconds.to_be_bytes());
            data.extend_from_slice(
                &u32::try_from(expires.nanos)
                    .map_err(|_| invalid_claim("certificate_p12_expiry_invalid"))?
                    .to_be_bytes(),
            );
            (
                118_u32,
                canonical_strings_and_bytes(
                    &[secret.key_id.as_str()],
                    &data,
                    payload.certificate_version,
                )?,
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
            (
                119_u32,
                canonical_strings_and_bytes(
                    &[payload.reason.as_str()],
                    &payload.certificate_id,
                    payload.certificate_version,
                )?,
            )
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => (
            112_u32,
            canonical_session_payload(&payload.session_id, &payload.boot_id)?,
        ),
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            let mut out = Vec::with_capacity(4 + payload.ip.len());
            append_semantic_bytes(&mut out, payload.ip.as_bytes())?;
            (113_u32, out)
        }
        Some(command_envelope::Payload::UserCreate(payload)) => (
            101_u32,
            canonical_secret_payload(
                &payload.username,
                payload.sealed_password_v1.as_ref(),
                &payload.sealed_password,
                &payload.secret_key_id,
                payload.desired_revision,
            )?,
        ),
        Some(command_envelope::Payload::UserDisable(payload)) => (
            102_u32,
            canonical_named_payload(&payload.username, &[], payload.desired_revision)?,
        ),
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => (
            114_u32,
            canonical_secret_payload(
                &payload.username,
                payload.sealed_password_v1.as_ref(),
                &payload.sealed_password,
                &payload.secret_key_id,
                payload.desired_revision,
            )?,
        ),
        Some(command_envelope::Payload::GroupApply(payload)) => (
            115_u32,
            canonical_named_payload(
                &payload.group_name,
                &payload.members,
                payload.desired_revision,
            )?,
        ),
        Some(command_envelope::Payload::UserEnable(payload)) => (
            116_u32,
            canonical_named_payload(&payload.username, &[], payload.desired_revision)?,
        ),
        _ => return Err(AuthorizationError::PayloadUnsupported),
    };
    let mut hash = Sha256::new();
    match version {
        SemanticPayloadHashVersion::V1 => {
            hash.update(b"ocservia.command.semantic-hash.v1\0");
        }
        SemanticPayloadHashVersion::V2 => {
            hash.update(b"ocservia.command.semantic-hash.v2\0");
        }
        SemanticPayloadHashVersion::Unspecified => {
            return Err(invalid_claim("semantic_hash_version_unsupported"));
        }
    }
    hash.update(&envelope.node_id);
    hash.update(envelope.expected_revision.to_be_bytes());
    hash.update(payload_kind.to_be_bytes());
    hash.update(canonical_payload);
    Ok(hash.finalize().into())
}

fn canonical_named_payload(
    name: &str,
    values: &[String],
    revision: u64,
) -> Result<Vec<u8>, AuthorizationError> {
    validate_semantic_name(name)?;
    validate_semantic_revision(revision)?;
    let mut out = Vec::new();
    append_semantic_bytes(&mut out, name.as_bytes())?;
    for value in values {
        validate_semantic_name(value)?;
        append_semantic_bytes(&mut out, value.as_bytes())?;
    }
    out.extend_from_slice(&0_u32.to_be_bytes());
    out.extend_from_slice(&revision.to_be_bytes());
    Ok(out)
}

fn canonical_secret_payload(
    name: &str,
    secret: Option<&SealedSecretV1>,
    legacy_ciphertext: &[u8],
    legacy_key_id: &str,
    revision: u64,
) -> Result<Vec<u8>, AuthorizationError> {
    validate_semantic_name(name)?;
    validate_semantic_revision(revision)?;
    if !legacy_ciphertext.is_empty() || !legacy_key_id.is_empty() {
        return Err(invalid_claim("sealed_secret_invalid"));
    }
    let secret = secret.ok_or_else(|| invalid_claim("sealed_secret_missing"))?;
    let secret_bytes = canonical_sealed_secret(secret, SealedSecretPurpose::UserPassword)?;
    let mut out = Vec::new();
    append_semantic_bytes(&mut out, name.as_bytes())?;
    append_semantic_bytes(&mut out, secret.key_id.as_bytes())?;
    append_semantic_bytes(&mut out, &secret_bytes)?;
    out.extend_from_slice(&revision.to_be_bytes());
    Ok(out)
}

fn canonical_sealed_secret(
    secret: &SealedSecretV1,
    expected_purpose: SealedSecretPurpose,
) -> Result<Vec<u8>, AuthorizationError> {
    if SealedSecretVersion::try_from(secret.version).ok() != Some(SealedSecretVersion::V1)
        || SealedSecretPurpose::try_from(secret.purpose).ok() != Some(expected_purpose)
        || secret.key_id.is_empty()
        || secret.key_id.len() > 128
        || secret.ciphertext.len() < 32
        || secret.ciphertext.len() > 16 * 1024
    {
        return Err(invalid_claim("sealed_secret_invalid"));
    }
    let mut out = Vec::with_capacity(8 + secret.ciphertext.len());
    out.extend_from_slice(
        &u32::try_from(secret.version)
            .map_err(|_| invalid_claim("sealed_secret_invalid"))?
            .to_be_bytes(),
    );
    out.extend_from_slice(
        &u32::try_from(secret.purpose)
            .map_err(|_| invalid_claim("sealed_secret_invalid"))?
            .to_be_bytes(),
    );
    out.extend_from_slice(&secret.ciphertext);
    Ok(out)
}

fn canonical_strings_and_bytes(
    values: &[&str],
    bytes: &[u8],
    revision: u64,
) -> Result<Vec<u8>, AuthorizationError> {
    let mut out = Vec::new();
    for value in values {
        append_semantic_bytes(&mut out, value.as_bytes())?;
    }
    append_semantic_bytes(&mut out, bytes)?;
    out.extend_from_slice(&revision.to_be_bytes());
    Ok(out)
}

fn canonical_session_payload(
    session_id: &str,
    boot_id: &str,
) -> Result<Vec<u8>, AuthorizationError> {
    let parsed = session_id
        .parse::<u64>()
        .map_err(|_| invalid_claim("session_id_invalid"))?;
    if parsed == 0 || parsed.to_string() != session_id || boot_id.is_empty() || boot_id.len() > 64 {
        return Err(invalid_claim("session_id_invalid"));
    }
    let mut out = Vec::with_capacity(8 + session_id.len() + boot_id.len());
    append_semantic_bytes(&mut out, session_id.as_bytes())?;
    append_semantic_bytes(&mut out, boot_id.as_bytes())?;
    Ok(out)
}

fn append_semantic_bytes(target: &mut Vec<u8>, value: &[u8]) -> Result<(), AuthorizationError> {
    target.extend_from_slice(
        &u32::try_from(value.len())
            .map_err(|_| invalid_claim("payload_size_invalid"))?
            .to_be_bytes(),
    );
    target.extend_from_slice(value);
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

/// Loads one PEM `SubjectPublicKeyInfo` through a descriptor-relative, no-follow path walk.
///
/// # Errors
///
/// Rejects unsafe ancestry, owner/mode/type/link violations, oversized files, and non-Ed25519 keys.
pub fn load_verification_key(
    path: &Path,
    expected_owner: u32,
    expected_group: u32,
) -> Result<VerifyingKey, io::Error> {
    if !path.is_absolute() {
        return Err(invalid(
            "Controller command verification key path must be absolute",
        ));
    }
    let mut components = path.components();
    if components.next() != Some(Component::RootDir) {
        return Err(invalid(
            "Controller command verification key path must be absolute",
        ));
    }
    let names = components
        .map(|component| match component {
            Component::Normal(name) => Ok(name),
            _ => Err(invalid(
                "Controller command verification key path must be clean",
            )),
        })
        .collect::<Result<Vec<_>, _>>()?;
    let (file_name, directories) = names
        .split_last()
        .ok_or_else(|| invalid("Controller command verification key path is invalid"))?;
    let root = rustix::fs::open(
        "/",
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
        Mode::empty(),
    )?;
    let mut directory = File::from(root);
    validate_directory(&directory, expected_owner)?;
    for name in directories {
        let next = rustix::fs::openat(
            &directory,
            *name,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )?;
        let next = File::from(next);
        validate_directory(&next, expected_owner)?;
        directory = next;
    }
    let key = rustix::fs::openat(
        &directory,
        *file_name,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )?;
    let mut key = File::from(key);
    let metadata = key.metadata()?;
    if !metadata.is_file() || metadata.nlink() != 1 {
        return Err(invalid(
            "Controller command verification key must be a one-link regular file",
        ));
    }
    let mode = metadata.permissions().mode() & 0o777;
    if !verification_key_metadata_allowed(
        metadata.uid(),
        metadata.gid(),
        mode,
        expected_owner,
        expected_group,
    ) {
        return Err(invalid(
            "Controller command verification key owner/mode must be process 0400/0600 or root:process-group 0440/0640",
        ));
    }
    let mut raw = Vec::with_capacity(256);
    key.by_ref().take(4097).read_to_end(&mut raw)?;
    if raw.is_empty() || raw.len() > 4096 {
        return Err(invalid(
            "Controller command verification key must contain 1..4096 bytes",
        ));
    }
    let text = std::str::from_utf8(&raw)
        .map_err(|_| invalid("Controller command verification key PEM must be UTF-8"))?;
    VerifyingKey::from_public_key_pem(text)
        .map_err(|_| invalid("Controller command verification key must be Ed25519 SPKI PEM"))
}

const fn verification_key_metadata_allowed(
    uid: u32,
    gid: u32,
    mode: u32,
    expected_owner: u32,
    expected_group: u32,
) -> bool {
    (uid == expected_owner && matches!(mode, 0o400 | 0o600))
        || (uid == 0 && gid == expected_group && matches!(mode, 0o440 | 0o640))
}

fn validate_directory(directory: &File, process_uid: u32) -> Result<(), io::Error> {
    let metadata = directory.metadata()?;
    if !metadata.is_dir()
        || (metadata.uid() != 0 && metadata.uid() != process_uid)
        || metadata.permissions().mode() & 0o022 != 0
    {
        return Err(invalid(
            "Controller command verification key ancestry must be root/process-owned and not group/world writable",
        ));
    }
    Ok(())
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
        _ => return Err(AuthorizationError::PayloadUnsupported),
    })
}

fn append_string(target: &mut Vec<u8>, value: &str) -> Result<(), AuthorizationError> {
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

fn fixed<const N: usize>(value: &[u8], code: &'static str) -> Result<[u8; N], AuthorizationError> {
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

fn timestamp_nanos(value: i32) -> Result<u32, AuthorizationError> {
    let value = u32::try_from(value)
        .map_err(|_| AuthorizationError::ClaimsInvalid("command_authorization_time_invalid"))?;
    if value >= 1_000_000_000 {
        return Err(AuthorizationError::ClaimsInvalid(
            "command_authorization_time_invalid",
        ));
    }
    Ok(value)
}

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::PermissionDenied, message)
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::{PermissionsExt as _, symlink};
    use std::time::{SystemTime, UNIX_EPOCH};

    use ed25519_dalek::SigningKey;
    use ed25519_dalek::pkcs8::{EncodePublicKey as _, spki::der::pem::LineEnding};
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        CommandAuthorizationProof, ConfigApply, SyntheticEcho, command_envelope,
    };
    use prost_types::Timestamp;
    use serde_json::Value;

    use super::*;

    #[test]
    fn rust_verifies_go_signatures_and_shared_canonical_vectors() {
        let fixture = load_fixture();
        assert_eq!(number(&fixture, "version"), 1);
        let public_key = VerifyingKey::from_bytes(&fixed::<32>(string(&fixture, "public_key_hex")))
            .expect("fixture public key");
        assert_eq!(verification_key_id(&public_key), string(&fixture, "key_id"));
        let keyring = ControllerCommandKeyring::new([public_key]).expect("fixture keyring");

        for vector in fixture["vectors"].as_array().expect("fixture vectors") {
            let claims = claims_from_fixture(&fixture, vector);
            let canonical = canonical_v1(&claims).expect("canonical signing input");
            assert_eq!(
                hex::encode(&canonical),
                string(vector, "canonical_preimage_hex"),
                "{} canonical mismatch",
                string(vector, "name")
            );
            let mut envelope = envelope_from_fixture(&claims);
            envelope.authorization = Some(CommandAuthorizationProof {
                version: CommandAuthorizationVersion::V1.into(),
                key_id: claims.key_id.clone(),
                signature: hex::decode(string(vector, "signature_hex")).expect("signature hex"),
            });
            keyring
                .verify(&envelope)
                .unwrap_or_else(|error| panic!("{} Go signature: {error}", string(vector, "name")));
        }
    }

    #[test]
    fn rust_verifies_go_session_grant_and_shared_canonical_vector() {
        let path =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/session-grant-v1.json");
        let raw = fs::read_to_string(&path)
            .unwrap_or_else(|error| panic!("read {}: {error}", path.display()));
        let fixture: Value = serde_json::from_str(&raw).expect("session grant fixture");
        let public_key = VerifyingKey::from_bytes(&fixed::<32>(string(&fixture, "public_key_hex")))
            .expect("fixture public key");
        let keyring = ControllerCommandKeyring::new([public_key]).expect("fixture keyring");
        let capabilities = fixture["negotiated_capabilities"]
            .as_array()
            .expect("capability array")
            .iter()
            .map(|value| value.as_str().expect("capability string").to_owned())
            .collect::<Vec<_>>();
        let claims = SessionGrantClaimsV1 {
            version: u32::try_from(number(&fixture, "version")).expect("version"),
            key_id: string(&fixture, "key_id").to_owned(),
            protocol_major: u32::try_from(number(&fixture, "protocol_major"))
                .expect("protocol major"),
            protocol_minor: u32::try_from(number(&fixture, "protocol_minor"))
                .expect("protocol minor"),
            node_id: fixed::<16>(string(&fixture, "node_id_hex")),
            endpoint_id: fixed::<32>(string(&fixture, "endpoint_id_hex")),
            authorization_revision: number(&fixture, "authorization_revision"),
            negotiated_capabilities: capabilities,
            issued_at_seconds: signed_number(&fixture, "issued_at_seconds"),
            issued_at_nanos: u32::try_from(number(&fixture, "issued_at_nanos"))
                .expect("issued nanos"),
            expires_at_seconds: signed_number(&fixture, "expires_at_seconds"),
            expires_at_nanos: u32::try_from(number(&fixture, "expires_at_nanos"))
                .expect("expiry nanos"),
        };
        let canonical = canonical_session_grant_v1(&claims).expect("canonical session grant");
        assert_eq!(
            hex::encode(&canonical),
            string(&fixture, "canonical_preimage_hex")
        );
        let mut grant = SessionGrantV1 {
            version: SessionGrantVersion::V1.into(),
            key_id: claims.key_id.clone(),
            protocol_major: claims.protocol_major,
            protocol_minor: claims.protocol_minor,
            node_id: claims.node_id.to_vec(),
            endpoint_id: claims.endpoint_id.to_vec(),
            authorization_revision: claims.authorization_revision,
            negotiated_capabilities: claims.negotiated_capabilities.clone(),
            issued_at: Some(Timestamp {
                seconds: claims.issued_at_seconds,
                nanos: i32::try_from(claims.issued_at_nanos).expect("issued nanos"),
            }),
            expires_at: Some(Timestamp {
                seconds: claims.expires_at_seconds,
                nanos: i32::try_from(claims.expires_at_nanos).expect("expiry nanos"),
            }),
            signature: hex::decode(string(&fixture, "signature_hex")).expect("signature hex"),
        };
        let verified = keyring
            .verify_session_grant(
                &grant,
                &claims.node_id,
                &claims.endpoint_id,
                claims.issued_at_seconds,
            )
            .expect("Go-signed session grant");
        assert_eq!(
            verified.authorization_revision,
            claims.authorization_revision
        );
        assert_eq!(
            keyring.verify_session_grant(
                &grant,
                &claims.node_id,
                &claims.endpoint_id,
                claims.expires_at_seconds,
            ),
            Err(AuthorizationError::ClaimsInvalid("session_grant_expired"))
        );

        let mut unknown_key = grant.clone();
        unknown_key.key_id = "ed25519-sha256:unknown".to_owned();
        assert_eq!(
            keyring.verify_session_grant(
                &unknown_key,
                &claims.node_id,
                &claims.endpoint_id,
                claims.issued_at_seconds,
            ),
            Err(AuthorizationError::UnknownKey)
        );

        grant.authorization_revision += 1;
        assert_eq!(
            keyring.verify_session_grant(
                &grant,
                &claims.node_id,
                &claims.endpoint_id,
                claims.issued_at_seconds,
            ),
            Err(AuthorizationError::SignatureInvalid)
        );
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn rust_verifies_go_artifact_grant_and_rejects_modified_or_expired_claims() {
        type ArtifactMutation = (&'static str, fn(&mut ArtifactGrantV1));

        let path =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/artifact-grant-v1.json");
        let raw = fs::read_to_string(&path)
            .unwrap_or_else(|error| panic!("read {}: {error}", path.display()));
        let fixture: Value = serde_json::from_str(&raw).expect("artifact grant fixture");
        let public_key = VerifyingKey::from_bytes(&fixed::<32>(string(&fixture, "public_key_hex")))
            .expect("fixture public key");
        let keyring = ControllerCommandKeyring::new([public_key]).expect("fixture keyring");
        let claims = ArtifactGrantClaimsV1 {
            version: u32::try_from(number(&fixture, "version")).expect("version"),
            key_id: string(&fixture, "key_id").to_owned(),
            node_id: fixed::<16>(string(&fixture, "node_id_hex")),
            artifact_id: fixed::<16>(string(&fixture, "artifact_id_hex")),
            certificate_id: fixed::<16>(string(&fixture, "certificate_id_hex")),
            certificate_version: number(&fixture, "certificate_version"),
            operation_id: fixed::<16>(string(&fixture, "operation_id_hex")),
            authorized_subject: string(&fixture, "authorized_subject").to_owned(),
            purpose: string(&fixture, "purpose").to_owned(),
            max_bytes: number(&fixture, "max_bytes"),
            issued_at_seconds: signed_number(&fixture, "issued_at_seconds"),
            issued_at_nanos: u32::try_from(number(&fixture, "issued_at_nanos"))
                .expect("issued nanos"),
            expires_at_seconds: signed_number(&fixture, "expires_at_seconds"),
            expires_at_nanos: u32::try_from(number(&fixture, "expires_at_nanos"))
                .expect("expiry nanos"),
            grant_id: fixed::<16>(string(&fixture, "grant_id_hex")),
        };
        let canonical = canonical_artifact_grant_v1(&claims).expect("canonical artifact grant");
        assert_eq!(
            hex::encode(canonical),
            string(&fixture, "canonical_preimage_hex")
        );
        let grant = ArtifactGrantV1 {
            version: ArtifactGrantVersion::V1.into(),
            key_id: claims.key_id.clone(),
            node_id: claims.node_id.to_vec(),
            artifact_id: claims.artifact_id.to_vec(),
            certificate_id: claims.certificate_id.to_vec(),
            certificate_version: claims.certificate_version,
            operation_id: claims.operation_id.to_vec(),
            authorized_subject: claims.authorized_subject.clone(),
            purpose: claims.purpose.clone(),
            max_bytes: claims.max_bytes,
            issued_at: Some(Timestamp {
                seconds: claims.issued_at_seconds,
                nanos: i32::try_from(claims.issued_at_nanos).expect("issued nanos"),
            }),
            expires_at: Some(Timestamp {
                seconds: claims.expires_at_seconds,
                nanos: i32::try_from(claims.expires_at_nanos).expect("expiry nanos"),
            }),
            grant_id: claims.grant_id.to_vec(),
            signature: hex::decode(string(&fixture, "signature_hex")).expect("signature hex"),
        };
        keyring
            .verify_artifact_grant(
                &grant,
                &claims.node_id,
                &claims.artifact_id,
                &claims.purpose,
                claims.max_bytes,
                claims.issued_at_seconds,
            )
            .expect("Go-signed artifact grant");
        assert_eq!(
            keyring.verify_artifact_grant(
                &grant,
                &claims.node_id,
                &claims.artifact_id,
                &claims.purpose,
                claims.max_bytes,
                claims.expires_at_seconds,
            ),
            Err(AuthorizationError::ClaimsInvalid("artifact_grant_expired"))
        );
        keyring
            .verify_artifact_grant_for_confirmation(
                &grant,
                &claims.node_id,
                &claims.artifact_id,
                &claims.purpose,
                claims.max_bytes,
                claims.expires_at_seconds,
            )
            .expect("expired exact grant remains valid only for read-only confirmation");
        let mut forged_confirmation = grant.clone();
        forged_confirmation.signature[0] ^= 1;
        assert_eq!(
            keyring.verify_artifact_grant_for_confirmation(
                &forged_confirmation,
                &claims.node_id,
                &claims.artifact_id,
                &claims.purpose,
                claims.max_bytes,
                claims.expires_at_seconds,
            ),
            Err(AuthorizationError::SignatureInvalid)
        );

        let mut unknown_key = grant.clone();
        unknown_key.key_id = "ed25519-sha256:unknown".to_owned();
        assert_eq!(
            keyring.verify_artifact_grant(
                &unknown_key,
                &claims.node_id,
                &claims.artifact_id,
                &claims.purpose,
                claims.max_bytes,
                claims.issued_at_seconds,
            ),
            Err(AuthorizationError::UnknownKey)
        );

        let mut wrong_node = claims.node_id;
        wrong_node[0] ^= 1;
        assert_eq!(
            keyring.verify_artifact_grant(
                &grant,
                &wrong_node,
                &claims.artifact_id,
                &claims.purpose,
                claims.max_bytes,
                claims.issued_at_seconds,
            ),
            Err(AuthorizationError::ClaimsInvalid(
                "artifact_grant_node_mismatch"
            ))
        );

        let mutations: [ArtifactMutation; 4] = [
            ("certificate version", |value: &mut ArtifactGrantV1| {
                value.certificate_version += 1;
            }),
            ("operation", |value: &mut ArtifactGrantV1| {
                value.operation_id[0] ^= 1;
            }),
            ("grant ID", |value: &mut ArtifactGrantV1| {
                value.grant_id[0] ^= 1;
            }),
            ("signature", |value: &mut ArtifactGrantV1| {
                value.signature[0] ^= 1;
            }),
        ];
        for (name, mutate) in mutations {
            let mut modified = grant.clone();
            mutate(&mut modified);
            assert_eq!(
                keyring.verify_artifact_grant(
                    &modified,
                    &claims.node_id,
                    &claims.artifact_id,
                    &claims.purpose,
                    claims.max_bytes,
                    claims.issued_at_seconds,
                ),
                Err(AuthorizationError::SignatureInvalid),
                "{name} mutation"
            );
        }
    }

    #[test]
    fn verification_key_loader_rejects_unsafe_paths() {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let directory = std::env::current_dir()
            .expect("current directory")
            .join(format!(".command-key-test-{}-{suffix}", std::process::id()));
        fs::create_dir(&directory).expect("create test directory");
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))
            .expect("secure directory mode");
        let key_path = directory.join("controller.pem");
        let key = SigningKey::from_bytes(&[7; 32]).verifying_key();
        fs::write(
            &key_path,
            key.to_public_key_pem(LineEnding::LF).expect("public PEM"),
        )
        .expect("write public key");
        fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).expect("secure key mode");
        let uid = rustix::process::geteuid().as_raw();
        let gid = rustix::process::getegid().as_raw();
        assert_eq!(
            load_verification_key(&key_path, uid, gid).expect("safe key"),
            key
        );

        fs::set_permissions(&key_path, fs::Permissions::from_mode(0o644)).expect("unsafe key mode");
        assert!(load_verification_key(&key_path, uid, gid).is_err());
        fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600))
            .expect("restore key mode");
        let link_path = directory.join("controller-link.pem");
        symlink(&key_path, &link_path).expect("key symlink");
        assert!(load_verification_key(&link_path, uid, gid).is_err());
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o770))
            .expect("unsafe ancestry mode");
        assert!(load_verification_key(&key_path, uid, gid).is_err());

        fs::remove_file(&link_path).expect("remove link");
        fs::remove_file(&key_path).expect("remove key");
        fs::remove_dir(&directory).expect("remove test directory");
    }

    #[test]
    fn shared_agent_privd_key_requires_root_group_readable_metadata() {
        let agent_owner = 997;
        let agent_group = 998;
        let allowed_for_agent = |uid, gid, mode| {
            verification_key_metadata_allowed(uid, gid, mode, agent_owner, agent_group)
        };
        let allowed_for_privd =
            |uid, gid, mode| verification_key_metadata_allowed(uid, gid, mode, 0, agent_group);

        for mode in [0o440, 0o640] {
            assert!(allowed_for_agent(0, agent_group, mode));
            assert!(allowed_for_privd(0, agent_group, mode));
        }

        for mode in [0o400, 0o600] {
            assert!(allowed_for_agent(agent_owner, agent_group, mode));
            assert!(!allowed_for_privd(agent_owner, agent_group, mode));
            assert!(!allowed_for_agent(0, agent_group, mode));
            assert!(allowed_for_privd(0, agent_group, mode));
        }

        assert!(!allowed_for_agent(0, agent_group + 1, 0o640));
        assert!(!allowed_for_privd(0, agent_group + 1, 0o640));
    }

    fn load_fixture() -> Value {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata/command-authorization-v1.json");
        let raw = fs::read_to_string(&path)
            .unwrap_or_else(|error| panic!("read {}: {error}", path.display()));
        serde_json::from_str(&raw)
            .unwrap_or_else(|error| panic!("parse {}: {error}", path.display()))
    }

    fn claims_from_fixture(document: &Value, vector: &Value) -> CommandAuthorizationV1 {
        CommandAuthorizationV1 {
            authorization_version: 1,
            key_id: string(document, "key_id").to_owned(),
            protocol_version: string(vector, "protocol_version").to_owned(),
            command_id: fixed::<16>(string(vector, "command_id_hex")),
            idempotency_key: fixed::<16>(string(vector, "idempotency_key_hex")),
            node_id: fixed::<16>(string(vector, "node_id_hex")),
            operation_id: fixed::<16>(string(vector, "operation_id_hex")),
            actor_identity: string(vector, "actor_identity").to_owned(),
            action: string(vector, "action").to_owned(),
            required_capability: string(vector, "required_capability").to_owned(),
            approval_id: optional_fixed_hex::<16>(string(vector, "approval_id_hex")),
            approval_request_sha256: optional_fixed_hex::<32>(string(
                vector,
                "approval_request_sha256_hex",
            )),
            expected_revision: number(vector, "expected_revision"),
            semantic_hash_version: u32::try_from(number(vector, "semantic_hash_version"))
                .expect("semantic version"),
            semantic_payload_sha256: fixed::<32>(string(vector, "semantic_payload_sha256_hex")),
            payload_kind: u32::try_from(number(vector, "payload_kind")).expect("payload kind"),
            delivery_mode: u32::try_from(number(vector, "delivery_mode")).expect("delivery mode"),
            issued_at_seconds: signed_number(vector, "issued_at_seconds"),
            issued_at_nanos: u32::try_from(number(vector, "issued_at_nanos"))
                .expect("issued nanos"),
            expires_at_seconds: signed_number(vector, "expires_at_seconds"),
            expires_at_nanos: u32::try_from(number(vector, "expires_at_nanos"))
                .expect("expiry nanos"),
        }
    }

    fn envelope_from_fixture(claims: &CommandAuthorizationV1) -> CommandEnvelope {
        let payload = match claims.payload_kind {
            104 => command_envelope::Payload::ConfigApply(ConfigApply::default()),
            108 => command_envelope::Payload::SyntheticEcho(SyntheticEcho::default()),
            kind => panic!("unsupported fixture payload kind {kind}"),
        };
        CommandEnvelope {
            protocol_version: claims.protocol_version.clone(),
            command_id: claims.command_id.to_vec(),
            idempotency_key: claims.idempotency_key.to_vec(),
            node_id: claims.node_id.to_vec(),
            operation_id: claims.operation_id.to_vec(),
            issued_at: Some(Timestamp {
                seconds: claims.issued_at_seconds,
                nanos: i32::try_from(claims.issued_at_nanos).expect("issued nanos"),
            }),
            expires_at: Some(Timestamp {
                seconds: claims.expires_at_seconds,
                nanos: i32::try_from(claims.expires_at_nanos).expect("expiry nanos"),
            }),
            expected_revision: claims.expected_revision,
            actor_id: claims.actor_identity.clone(),
            action: claims.action.clone(),
            required_capability: claims.required_capability.clone(),
            approval_id: claims
                .approval_id
                .map_or_else(Vec::new, |value| value.to_vec()),
            approval_request_sha256: claims
                .approval_request_sha256
                .map_or_else(Vec::new, |value| value.to_vec()),
            semantic_payload_hash_version: i32::try_from(claims.semantic_hash_version)
                .expect("semantic version"),
            semantic_payload_sha256: claims.semantic_payload_sha256.to_vec(),
            delivery_mode: i32::try_from(claims.delivery_mode).expect("delivery mode"),
            payload: Some(payload),
            authorization: None,
            ..CommandEnvelope::default()
        }
    }

    fn string<'a>(value: &'a Value, name: &str) -> &'a str {
        value[name]
            .as_str()
            .unwrap_or_else(|| panic!("fixture field {name} must be a string"))
    }

    fn number(value: &Value, name: &str) -> u64 {
        value[name]
            .as_u64()
            .unwrap_or_else(|| panic!("fixture field {name} must be u64"))
    }

    fn signed_number(value: &Value, name: &str) -> i64 {
        value[name]
            .as_i64()
            .unwrap_or_else(|| panic!("fixture field {name} must be i64"))
    }

    fn fixed<const N: usize>(value: &str) -> [u8; N] {
        hex::decode(value)
            .expect("fixture hex")
            .try_into()
            .unwrap_or_else(|_| panic!("fixture hex must contain {N} bytes"))
    }

    fn optional_fixed_hex<const N: usize>(value: &str) -> Option<[u8; N]> {
        (!value.is_empty()).then(|| fixed(value))
    }
}
