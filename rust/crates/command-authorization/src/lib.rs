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
    ArtifactGrantV1, ArtifactGrantVersion, CommandAuthorizationVersion, CommandDeliveryMode,
    CommandEnvelope, ConnectionFenceV2, FenceBindingV2, FenceOperationKind, FenceSignatureVersion,
    SealedSecretPurpose, SealedSecretV1, SealedSecretVersion, SemanticPayloadHashVersion,
    SessionGrantV1, SessionGrantVersion, command_envelope,
};
use rustix::fs::{Mode, OFlags};
use sha2::{Digest as _, Sha256};

/// The signed command revision with immutable execution deadlines.
pub const COMMAND_PROTOCOL_VERSION: &str = "1.2";
/// Legacy recovery grants may have lost their original execution deadline.
pub const LEGACY_COMMAND_PROTOCOL_VERSION: &str = "1.1";
const DOMAIN_SEPARATOR: &[u8] = b"ocservia/controller-command/v1\0";
const SESSION_GRANT_DOMAIN_SEPARATOR: &[u8] = b"ocservia/controller-session-grant/v1\0";
const ARTIFACT_GRANT_DOMAIN_SEPARATOR: &[u8] = b"ocservia/artifact-grant/v1\0";
const CONNECTION_FENCE_V2_DOMAIN_SEPARATOR: &[u8] = b"ocservia/connection-fence/v2\0";
const FENCE_BINDING_V2_DOMAIN_SEPARATOR: &[u8] = b"ocservia/fence-binding/v2\0";
const KEY_ID_PREFIX: &str = "ed25519-sha256:";
const MAX_FUTURE_SKEW_SECONDS: i64 = 300;

/// The only signature version of the frozen V2 fence contract: Ed25519 with
/// the sha256 key identifier scheme.
pub const FENCE_SIGNATURE_VERSION_ED25519_V1: u32 = 1;

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
        if claims.expires_at_seconds <= now_unix_seconds
            && envelope.delivery_mode != i32::from(CommandDeliveryMode::ReconcileOnly)
        {
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

    /// Verifies a Controller-signed per-node connection-owner fence for the
    /// local node and endpoint. transportd relays fences but cannot mint or
    /// alter them; the lease deadline bound here is the PostgreSQL-time
    /// deadline recorded by the database ownership authority.
    ///
    /// # Errors
    ///
    /// Returns an authorization error when the fence is malformed, expired,
    /// signed by an unknown key, has an invalid signature, or does not match
    /// the expected node and endpoint. This second-only compatibility API
    /// evaluates the supplied second at nanosecond zero; callers with a
    /// subsecond clock should use [`Self::verify_connection_fence_v2_at`].
    pub fn verify_connection_fence_v2(
        &self,
        fence: &ConnectionFenceV2,
        expected_node_id: &[u8; 16],
        expected_endpoint_id: &[u8; 32],
        now_unix_seconds: i64,
    ) -> Result<VerifiedConnectionFenceV2, AuthorizationError> {
        self.verify_connection_fence_v2_at(
            fence,
            expected_node_id,
            expected_endpoint_id,
            now_unix_seconds,
            0,
        )
    }

    /// Verifies a Controller-signed per-node connection-owner fence at an
    /// exact nanosecond-precision wall-clock instant.
    ///
    /// # Errors
    ///
    /// Returns an authorization error when the current nanosecond value is
    /// invalid or the fence is malformed, expired, future-issued beyond the
    /// allowed skew, signed by an unknown key, has an invalid signature, or
    /// does not match the expected node and endpoint.
    pub fn verify_connection_fence_v2_at(
        &self,
        fence: &ConnectionFenceV2,
        expected_node_id: &[u8; 16],
        expected_endpoint_id: &[u8; 32],
        now_unix_seconds: i64,
        now_unix_nanos: u32,
    ) -> Result<VerifiedConnectionFenceV2, AuthorizationError> {
        if now_unix_nanos >= 1_000_000_000 {
            return Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_clock_skew",
            ));
        }
        let claims = connection_fence_claims_v2(fence)?;
        let key = self
            .keys
            .get(&claims.key_id)
            .ok_or(AuthorizationError::UnknownKey)?;
        if &claims.node_id != expected_node_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_node_mismatch",
            ));
        }
        if &claims.endpoint_id != expected_endpoint_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_endpoint_mismatch",
            ));
        }
        if timestamp_expired(
            claims.expires_at_seconds,
            claims.expires_at_nanos,
            now_unix_seconds,
            now_unix_nanos,
        ) {
            return Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_expired",
            ));
        }
        if timestamp_exceeds_future_skew(
            claims.issued_at_seconds,
            claims.issued_at_nanos,
            now_unix_seconds,
            now_unix_nanos,
        ) {
            return Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_clock_skew",
            ));
        }
        let canonical = canonical_connection_fence_v2(&claims)?;
        let signature = Signature::from_slice(&fence.signature)
            .map_err(|_| AuthorizationError::SignatureMalformed)?;
        key.verify_strict(&canonical, &signature)
            .map_err(|_| AuthorizationError::SignatureInvalid)?;
        Ok(VerifiedConnectionFenceV2 {
            fence_id: claims.fence_id,
            owner_instance_id: claims.owner_instance_id,
            owner_incarnation: claims.owner_incarnation,
            owner_epoch: claims.owner_epoch,
            connection_id: claims.connection_id,
            authorization_revision: claims.authorization_revision,
            capabilities: claims.capabilities,
            lease_until_seconds: claims.lease_until_seconds,
            lease_until_nanos: claims.lease_until_nanos,
            expires_at_seconds: claims.expires_at_seconds,
            expires_at_nanos: claims.expires_at_nanos,
        })
    }

    /// Verifies a Controller-signed operation binding for the local node and
    /// endpoint. The caller matches the returned claims against the fence
    /// recorded on the actual dispatch attempt with
    /// [`FenceBindingClaimsV2::matches_fence`], so late legitimate results
    /// are accepted while results recorded under any other term are
    /// rejected.
    ///
    /// # Errors
    ///
    /// Returns an authorization error when the binding is malformed, expired,
    /// signed by an unknown key, has an invalid signature, or does not match
    /// the expected node and endpoint. This second-only compatibility API
    /// evaluates the supplied second at nanosecond zero; callers with a
    /// subsecond clock should use [`Self::verify_fence_binding_v2_at`].
    pub fn verify_fence_binding_v2(
        &self,
        binding: &FenceBindingV2,
        expected_node_id: &[u8; 16],
        expected_endpoint_id: &[u8; 32],
        now_unix_seconds: i64,
    ) -> Result<FenceBindingClaimsV2, AuthorizationError> {
        self.verify_fence_binding_v2_at(
            binding,
            expected_node_id,
            expected_endpoint_id,
            now_unix_seconds,
            0,
        )
    }

    /// Verifies a Controller-signed operation binding at an exact
    /// nanosecond-precision wall-clock instant.
    ///
    /// # Errors
    ///
    /// Returns an authorization error when the current nanosecond value is
    /// invalid or the binding is malformed, expired, future-issued beyond the
    /// allowed skew, signed by an unknown key, has an invalid signature, or
    /// does not match the expected node and endpoint.
    pub fn verify_fence_binding_v2_at(
        &self,
        binding: &FenceBindingV2,
        expected_node_id: &[u8; 16],
        expected_endpoint_id: &[u8; 32],
        now_unix_seconds: i64,
        now_unix_nanos: u32,
    ) -> Result<FenceBindingClaimsV2, AuthorizationError> {
        if now_unix_nanos >= 1_000_000_000 {
            return Err(AuthorizationError::ClaimsInvalid(
                "fence_binding_clock_skew",
            ));
        }
        let claims = fence_binding_claims_v2(binding)?;
        let key = self
            .keys
            .get(&claims.key_id)
            .ok_or(AuthorizationError::UnknownKey)?;
        if &claims.node_id != expected_node_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "fence_binding_node_mismatch",
            ));
        }
        if &claims.endpoint_id != expected_endpoint_id {
            return Err(AuthorizationError::ClaimsInvalid(
                "fence_binding_endpoint_mismatch",
            ));
        }
        if timestamp_expired(
            claims.expires_at_seconds,
            claims.expires_at_nanos,
            now_unix_seconds,
            now_unix_nanos,
        ) {
            return Err(AuthorizationError::ClaimsInvalid("fence_binding_expired"));
        }
        if timestamp_exceeds_future_skew(
            claims.issued_at_seconds,
            claims.issued_at_nanos,
            now_unix_seconds,
            now_unix_nanos,
        ) {
            return Err(AuthorizationError::ClaimsInvalid(
                "fence_binding_clock_skew",
            ));
        }
        let canonical = canonical_fence_binding_v2(&claims)?;
        let signature = Signature::from_slice(&binding.signature)
            .map_err(|_| AuthorizationError::SignatureMalformed)?;
        key.verify_strict(&canonical, &signature)
            .map_err(|_| AuthorizationError::SignatureInvalid)?;
        Ok(claims)
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

/// Independent, non-Protobuf canonical claims for one per-node
/// connection-owner fence term.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConnectionFenceClaimsV2 {
    pub signature_version: u32,
    pub key_id: String,
    pub fence_id: [u8; 16],
    pub node_id: [u8; 16],
    pub endpoint_id: [u8; 32],
    pub owner_instance_id: [u8; 16],
    pub owner_incarnation: u64,
    pub owner_epoch: u64,
    pub connection_id: [u8; 16],
    pub authorization_revision: u64,
    pub capabilities: Vec<String>,
    pub lease_until_seconds: i64,
    pub lease_until_nanos: u32,
    pub issued_at_seconds: i64,
    pub issued_at_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

/// Independent, non-Protobuf canonical claims binding one operation identity
/// to the exact fence term of its dispatch attempt.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FenceBindingClaimsV2 {
    pub signature_version: u32,
    pub key_id: String,
    pub operation_kind: u32,
    pub operation_id: [u8; 16],
    pub fence_id: [u8; 16],
    pub node_id: [u8; 16],
    pub endpoint_id: [u8; 32],
    pub owner_instance_id: [u8; 16],
    pub owner_incarnation: u64,
    pub owner_epoch: u64,
    pub connection_id: [u8; 16],
    pub authorization_revision: u64,
    pub capability: String,
    pub issued_at_seconds: i64,
    pub issued_at_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

/// A verified connection-owner fence term. transportd and the Agent use the
/// recorded owner tuple to match operation bindings against the fence of the
/// actual dispatch attempt. The lease deadline keeps its nanosecond part:
/// truncating it to whole seconds would reject leases that are still valid
/// for up to one second.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VerifiedConnectionFenceV2 {
    pub fence_id: [u8; 16],
    pub owner_instance_id: [u8; 16],
    pub owner_incarnation: u64,
    pub owner_epoch: u64,
    pub connection_id: [u8; 16],
    pub authorization_revision: u64,
    pub capabilities: Vec<String>,
    pub lease_until_seconds: i64,
    pub lease_until_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

impl VerifiedConnectionFenceV2 {
    /// Reports whether the ownership lease deadline has passed at the given
    /// nanosecond-precise instant.
    #[must_use]
    pub fn lease_expired(&self, now_seconds: i64, now_nanos: u32) -> bool {
        (self.lease_until_seconds, self.lease_until_nanos) <= (now_seconds, now_nanos)
    }
}

/// The domain separator of the canonical state-update operation identity.
/// One node-trust update is one operation: the Controller derives the
/// identity from the exact carrier and signs a fence binding over it, while
/// transportd derives it again independently, so a binding for one update
/// can never authorize a different endpoint, state, revision, or reason.
pub const STATE_UPDATE_OPERATION_DOMAIN: &[u8] = b"ocservia/state-update-operation/v1\0";

/// Derives the canonical operation identity of one node trust update: the
/// first 16 bytes of SHA-256 over the domain separator, node id, endpoint
/// id, state code, revision, byte length of the reason, and the reason.
#[must_use]
pub fn state_update_operation_id(
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    state: u32,
    revision: u64,
    reason: &str,
) -> [u8; 16] {
    let mut digest = Sha256::new();
    digest.update(STATE_UPDATE_OPERATION_DOMAIN);
    digest.update(node_id);
    digest.update(endpoint_id);
    digest.update(state.to_be_bytes());
    digest.update(revision.to_be_bytes());
    let reason_bytes = reason.as_bytes();
    digest.update(
        u32::try_from(reason_bytes.len())
            .unwrap_or(u32::MAX)
            .to_be_bytes(),
    );
    digest.update(reason_bytes);
    let digest = digest.finalize();
    let mut operation_id = [0_u8; 16];
    operation_id.copy_from_slice(&digest[..16]);
    operation_id
}

impl FenceBindingClaimsV2 {
    /// Reports whether this binding was signed for exactly the fence term of
    /// the recorded dispatch attempt. A late legitimate result still matches
    /// its own attempt's fence; a result recorded under any other epoch,
    /// connection, or owner does not.
    #[must_use]
    pub fn matches_fence(&self, fence: &VerifiedConnectionFenceV2) -> bool {
        self.fence_id == fence.fence_id
            && self.owner_instance_id == fence.owner_instance_id
            && self.owner_incarnation == fence.owner_incarnation
            && self.owner_epoch == fence.owner_epoch
            && self.connection_id == fence.connection_id
            && self.authorization_revision == fence.authorization_revision
    }
}

/// Projects a Controller-signed `ConnectionFenceV2` into the independent
/// claims model.
///
/// # Errors
///
/// Rejects unsupported signature versions and malformed fixed-width fields.
pub fn connection_fence_claims_v2(
    fence: &ConnectionFenceV2,
) -> Result<ConnectionFenceClaimsV2, AuthorizationError> {
    if signature_version_value(fence.signature_version) != Some(FENCE_SIGNATURE_VERSION_ED25519_V1)
    {
        return Err(AuthorizationError::UnsupportedVersion);
    }
    let lease_until = fence
        .lease_until
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "connection_fence_lease_until_missing",
        ))?;
    let issued_at = fence
        .issued_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "connection_fence_issued_at_missing",
        ))?;
    let expires_at = fence
        .expires_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "connection_fence_expires_at_missing",
        ))?;
    Ok(ConnectionFenceClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: fence.key_id.clone(),
        fence_id: fixed::<16>(&fence.fence_id, "connection_fence_fence_id_invalid")?,
        node_id: fixed::<16>(&fence.node_id, "connection_fence_node_invalid")?,
        endpoint_id: fixed::<32>(&fence.endpoint_id, "connection_fence_endpoint_invalid")?,
        owner_instance_id: fixed::<16>(
            &fence.owner_instance_id,
            "connection_fence_owner_instance_invalid",
        )?,
        owner_incarnation: fence.owner_incarnation,
        owner_epoch: fence.owner_epoch,
        connection_id: fixed::<16>(&fence.connection_id, "connection_fence_connection_invalid")?,
        authorization_revision: fence.authorization_revision,
        capabilities: fence.capabilities.clone(),
        lease_until_seconds: lease_until.seconds,
        lease_until_nanos: timestamp_nanos(lease_until.nanos)?,
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos: timestamp_nanos(issued_at.nanos)?,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos: timestamp_nanos(expires_at.nanos)?,
    })
}

/// Produces the exact bytes signed for `ConnectionFenceV2` without
/// serializing Protobuf. The domain separator is disjoint from every V1
/// contract, so V1 and V2 proofs can never cross-validate.
///
/// # Errors
///
/// Rejects invalid signature versions, key IDs, epochs, revisions,
/// timestamps, lease bounds, and unsorted or duplicate capabilities.
pub fn canonical_connection_fence_v2(
    claims: &ConnectionFenceClaimsV2,
) -> Result<Vec<u8>, AuthorizationError> {
    if claims.signature_version != FENCE_SIGNATURE_VERSION_ED25519_V1
        || claims.key_id.is_empty()
        || claims.key_id.len() > 128
        || claims.owner_epoch == 0
        || claims.authorization_revision == 0
        || claims.capabilities.len() > 128
        || claims.lease_until_nanos >= 1_000_000_000
        || claims.issued_at_nanos >= 1_000_000_000
        || claims.expires_at_nanos >= 1_000_000_000
        || !deadline_strictly_after(
            claims.lease_until_seconds,
            claims.lease_until_nanos,
            claims.issued_at_seconds,
            claims.issued_at_nanos,
        )
        || !deadline_not_after(
            claims.lease_until_seconds,
            claims.lease_until_nanos,
            claims.expires_at_seconds,
            claims.expires_at_nanos,
        )
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "connection_fence_claims_invalid",
        ));
    }
    let mut previous: Option<&str> = None;
    for capability in &claims.capabilities {
        if capability.is_empty()
            || capability.len() > 128
            || previous.is_some_and(|value| value >= capability.as_str())
        {
            return Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_capabilities_invalid",
            ));
        }
        previous = Some(capability);
    }
    let mut encoded = Vec::with_capacity(640);
    encoded.extend_from_slice(CONNECTION_FENCE_V2_DOMAIN_SEPARATOR);
    encoded.extend_from_slice(&claims.signature_version.to_be_bytes());
    append_string(&mut encoded, &claims.key_id)?;
    encoded.extend_from_slice(&claims.fence_id);
    encoded.extend_from_slice(&claims.node_id);
    encoded.extend_from_slice(&claims.endpoint_id);
    encoded.extend_from_slice(&claims.owner_instance_id);
    encoded.extend_from_slice(&claims.owner_incarnation.to_be_bytes());
    encoded.extend_from_slice(&claims.owner_epoch.to_be_bytes());
    encoded.extend_from_slice(&claims.connection_id);
    encoded.extend_from_slice(&claims.authorization_revision.to_be_bytes());
    encoded.extend_from_slice(
        &u32::try_from(claims.capabilities.len())
            .map_err(|_| {
                AuthorizationError::ClaimsInvalid("connection_fence_capabilities_invalid")
            })?
            .to_be_bytes(),
    );
    for capability in &claims.capabilities {
        append_string(&mut encoded, capability)?;
    }
    encoded.extend_from_slice(&claims.lease_until_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.lease_until_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_nanos.to_be_bytes());
    Ok(encoded)
}

/// Projects a Controller-signed `FenceBindingV2` into the independent claims
/// model.
///
/// # Errors
///
/// Rejects unsupported signature versions and malformed fixed-width fields.
pub fn fence_binding_claims_v2(
    binding: &FenceBindingV2,
) -> Result<FenceBindingClaimsV2, AuthorizationError> {
    if signature_version_value(binding.signature_version)
        != Some(FENCE_SIGNATURE_VERSION_ED25519_V1)
    {
        return Err(AuthorizationError::UnsupportedVersion);
    }
    let issued_at = binding
        .issued_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "fence_binding_issued_at_missing",
        ))?;
    let expires_at = binding
        .expires_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "fence_binding_expires_at_missing",
        ))?;
    Ok(FenceBindingClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: binding.key_id.clone(),
        operation_kind: fence_operation_kind_value(binding.operation_kind)?,
        operation_id: fixed::<16>(&binding.operation_id, "fence_binding_operation_invalid")?,
        fence_id: fixed::<16>(&binding.fence_id, "fence_binding_fence_id_invalid")?,
        node_id: fixed::<16>(&binding.node_id, "fence_binding_node_invalid")?,
        endpoint_id: fixed::<32>(&binding.endpoint_id, "fence_binding_endpoint_invalid")?,
        owner_instance_id: fixed::<16>(
            &binding.owner_instance_id,
            "fence_binding_owner_instance_invalid",
        )?,
        owner_incarnation: binding.owner_incarnation,
        owner_epoch: binding.owner_epoch,
        connection_id: fixed::<16>(&binding.connection_id, "fence_binding_connection_invalid")?,
        authorization_revision: binding.authorization_revision,
        capability: binding.capability.clone(),
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos: timestamp_nanos(issued_at.nanos)?,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos: timestamp_nanos(expires_at.nanos)?,
    })
}

/// Produces the exact bytes signed for `FenceBindingV2` without serializing
/// Protobuf.
///
/// # Errors
///
/// Rejects invalid signature versions, key IDs, operation kinds, epochs,
/// revisions, capabilities, and timestamps.
pub fn canonical_fence_binding_v2(
    claims: &FenceBindingClaimsV2,
) -> Result<Vec<u8>, AuthorizationError> {
    if claims.signature_version != FENCE_SIGNATURE_VERSION_ED25519_V1
        || claims.key_id.is_empty()
        || claims.key_id.len() > 128
        || claims.operation_kind == 0
        || claims.operation_kind > 4
        || claims.owner_epoch == 0
        || claims.authorization_revision == 0
        || claims.capability.is_empty()
        || claims.capability.len() > 128
        || claims.issued_at_nanos >= 1_000_000_000
        || claims.expires_at_nanos >= 1_000_000_000
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "fence_binding_claims_invalid",
        ));
    }
    let mut encoded = Vec::with_capacity(512);
    encoded.extend_from_slice(FENCE_BINDING_V2_DOMAIN_SEPARATOR);
    encoded.extend_from_slice(&claims.signature_version.to_be_bytes());
    append_string(&mut encoded, &claims.key_id)?;
    encoded.extend_from_slice(&claims.operation_kind.to_be_bytes());
    encoded.extend_from_slice(&claims.operation_id);
    encoded.extend_from_slice(&claims.fence_id);
    encoded.extend_from_slice(&claims.node_id);
    encoded.extend_from_slice(&claims.endpoint_id);
    encoded.extend_from_slice(&claims.owner_instance_id);
    encoded.extend_from_slice(&claims.owner_incarnation.to_be_bytes());
    encoded.extend_from_slice(&claims.owner_epoch.to_be_bytes());
    encoded.extend_from_slice(&claims.connection_id);
    encoded.extend_from_slice(&claims.authorization_revision.to_be_bytes());
    append_string(&mut encoded, &claims.capability)?;
    encoded.extend_from_slice(&claims.issued_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_nanos.to_be_bytes());
    Ok(encoded)
}

fn signature_version_value(value: i32) -> Option<u32> {
    match FenceSignatureVersion::try_from(value) {
        Ok(FenceSignatureVersion::Ed25519V1) => Some(FENCE_SIGNATURE_VERSION_ED25519_V1),
        _ => None,
    }
}

fn fence_operation_kind_value(value: i32) -> Result<u32, AuthorizationError> {
    match FenceOperationKind::try_from(value) {
        Ok(FenceOperationKind::Command) => Ok(1),
        Ok(FenceOperationKind::Artifact) => Ok(2),
        Ok(FenceOperationKind::ConnectionClose) => Ok(3),
        Ok(FenceOperationKind::StateUpdate) => Ok(4),
        _ => Err(AuthorizationError::ClaimsInvalid(
            "fence_binding_operation_kind_invalid",
        )),
    }
}

fn deadline_strictly_after(
    until_seconds: i64,
    until_nanos: u32,
    from_seconds: i64,
    from_nanos: u32,
) -> bool {
    until_seconds > from_seconds || (until_seconds == from_seconds && until_nanos > from_nanos)
}

fn deadline_not_after(
    until_seconds: i64,
    until_nanos: u32,
    limit_seconds: i64,
    limit_nanos: u32,
) -> bool {
    until_seconds < limit_seconds || (until_seconds == limit_seconds && until_nanos <= limit_nanos)
}

fn timestamp_expired(
    expires_at_seconds: i64,
    expires_at_nanos: u32,
    now_seconds: i64,
    now_nanos: u32,
) -> bool {
    (expires_at_seconds, expires_at_nanos) <= (now_seconds, now_nanos)
}

fn timestamp_exceeds_future_skew(
    issued_at_seconds: i64,
    issued_at_nanos: u32,
    now_seconds: i64,
    now_nanos: u32,
) -> bool {
    now_seconds
        .checked_add(MAX_FUTURE_SKEW_SECONDS)
        .is_some_and(|limit_seconds| {
            (issued_at_seconds, issued_at_nanos) > (limit_seconds, now_nanos)
        })
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
            (129_u32, payload.candidate_hash.clone())
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
            let mut out = payload.candidate_hash.clone();
            out.extend_from_slice(&payload.materialized_hash);
            out.extend_from_slice(&payload.expected_current_hash);
            out.extend_from_slice(&payload.desired_revision.to_be_bytes());
            out.extend_from_slice(&payload.plan_id);
            out.extend_from_slice(&expiry.seconds.to_be_bytes());
            out.extend_from_slice(&expiry.nanos.to_be_bytes());
            out.extend_from_slice(&envelope.approval_id);
            out.extend_from_slice(&envelope.approval_request_sha256);
            (130_u32, out)
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
        Some(command_envelope::Payload::AgentUpgrade(payload)) => {
            if !ocservia_contracts::agent_upgrade::valid_target_version(&payload.target_version)
                || payload.package_sha256.len() != 32
                || !ocservia_contracts::agent_upgrade::valid_architecture(&payload.architecture)
            {
                return Err(invalid_claim("agent_upgrade_invalid"));
            }
            (
                128_u32,
                canonical_strings_and_bytes(
                    &[
                        payload.target_version.as_str(),
                        payload.architecture.as_str(),
                    ],
                    &payload.package_sha256,
                    0,
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
#[allow(clippy::too_many_lines)]
mod tests {
    use std::fs;
    use std::os::unix::fs::symlink;
    use std::time::{SystemTime, UNIX_EPOCH};

    use ed25519_dalek::pkcs8::{EncodePublicKey as _, spki::der::pem::LineEnding};
    use ed25519_dalek::{Signer as _, SigningKey};
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        AgentUpgrade, CommandAuthorizationProof, ConfigApply, SyntheticEcho, command_envelope,
        complete_config_directive,
    };
    use prost_types::Timestamp;
    use serde_json::Value;

    use super::*;

    type ArtifactMutation = (&'static str, fn(&mut ArtifactGrantV1));

    fn load_fixture(name: &str) -> Value {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../testdata")
            .join(name);
        let raw =
            fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
        serde_json::from_str(&raw).expect("fixture json")
    }
    fn s<'a>(v: &'a Value, k: &str) -> &'a str {
        v[k].as_str().unwrap_or_else(|| panic!("{k} string"))
    }
    fn u(v: &Value, k: &str) -> u64 {
        v[k].as_u64().unwrap_or_else(|| panic!("{k} u64"))
    }
    fn i(v: &Value, k: &str) -> i64 {
        v[k].as_i64().unwrap_or_else(|| panic!("{k} i64"))
    }
    fn u32v(v: &Value, k: &str) -> u32 {
        u32::try_from(u(v, k)).expect("u32")
    }
    fn fixed<const N: usize>(hex_s: &str) -> [u8; N] {
        hex::decode(hex_s).expect("hex").try_into().expect("len")
    }
    fn opt_fixed<const N: usize>(hex_s: &str) -> Option<[u8; N]> {
        (!hex_s.is_empty()).then(|| fixed(hex_s))
    }
    #[allow(clippy::unnecessary_wraps)]
    fn ts(sec: i64, nanos: u32) -> Option<Timestamp> {
        Some(Timestamp {
            seconds: sec,
            nanos: i32::try_from(nanos).expect("nanos"),
        })
    }
    fn fixture_keyring(f: &Value) -> ControllerCommandKeyring {
        let pk = VerifyingKey::from_bytes(&fixed::<32>(s(f, "public_key_hex"))).expect("pk");
        ControllerCommandKeyring::new([pk]).expect("keyring")
    }
    fn str_vec(v: &Value, k: &str) -> Vec<String> {
        v[k].as_array()
            .expect("arr")
            .iter()
            .map(|x| x.as_str().expect("s").to_owned())
            .collect()
    }

    fn fence_from_fixture(f: &Value) -> (ConnectionFenceClaimsV2, ConnectionFenceV2) {
        let c = ConnectionFenceClaimsV2 {
            signature_version: u32v(f, "signature_version"),
            key_id: s(f, "key_id").into(),
            fence_id: fixed(s(f, "fence_id_hex")),
            node_id: fixed(s(f, "node_id_hex")),
            endpoint_id: fixed(s(f, "endpoint_id_hex")),
            owner_instance_id: fixed(s(f, "owner_instance_id_hex")),
            owner_incarnation: u(f, "owner_incarnation"),
            owner_epoch: u(f, "owner_epoch"),
            connection_id: fixed(s(f, "connection_id_hex")),
            authorization_revision: u(f, "authorization_revision"),
            capabilities: str_vec(f, "capabilities"),
            lease_until_seconds: i(f, "lease_until_seconds"),
            lease_until_nanos: u32v(f, "lease_until_nanos"),
            issued_at_seconds: i(f, "issued_at_seconds"),
            issued_at_nanos: u32v(f, "issued_at_nanos"),
            expires_at_seconds: i(f, "expires_at_seconds"),
            expires_at_nanos: u32v(f, "expires_at_nanos"),
        };
        let m = ConnectionFenceV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: c.key_id.clone(),
            fence_id: c.fence_id.to_vec(),
            node_id: c.node_id.to_vec(),
            endpoint_id: c.endpoint_id.to_vec(),
            owner_instance_id: c.owner_instance_id.to_vec(),
            owner_incarnation: c.owner_incarnation,
            owner_epoch: c.owner_epoch,
            connection_id: c.connection_id.to_vec(),
            authorization_revision: c.authorization_revision,
            capabilities: c.capabilities.clone(),
            lease_until: ts(c.lease_until_seconds, c.lease_until_nanos),
            issued_at: ts(c.issued_at_seconds, c.issued_at_nanos),
            expires_at: ts(c.expires_at_seconds, c.expires_at_nanos),
            signature: hex::decode(s(f, "signature_hex")).expect("sig"),
        };
        (c, m)
    }

    fn binding_from_fixture(f: &Value) -> (FenceBindingClaimsV2, FenceBindingV2) {
        let c = FenceBindingClaimsV2 {
            signature_version: u32v(f, "signature_version"),
            key_id: s(f, "key_id").into(),
            operation_kind: u32v(f, "operation_kind"),
            operation_id: fixed(s(f, "operation_id_hex")),
            fence_id: fixed(s(f, "fence_id_hex")),
            node_id: fixed(s(f, "node_id_hex")),
            endpoint_id: fixed(s(f, "endpoint_id_hex")),
            owner_instance_id: fixed(s(f, "owner_instance_id_hex")),
            owner_incarnation: u(f, "owner_incarnation"),
            owner_epoch: u(f, "owner_epoch"),
            connection_id: fixed(s(f, "connection_id_hex")),
            authorization_revision: u(f, "authorization_revision"),
            capability: s(f, "capability").into(),
            issued_at_seconds: i(f, "issued_at_seconds"),
            issued_at_nanos: u32v(f, "issued_at_nanos"),
            expires_at_seconds: i(f, "expires_at_seconds"),
            expires_at_nanos: u32v(f, "expires_at_nanos"),
        };
        let m = FenceBindingV2 {
            signature_version: FenceSignatureVersion::Ed25519V1.into(),
            key_id: c.key_id.clone(),
            operation_kind: FenceOperationKind::try_from(
                i32::try_from(c.operation_kind).expect("k"),
            )
            .expect("k")
            .into(),
            operation_id: c.operation_id.to_vec(),
            fence_id: c.fence_id.to_vec(),
            node_id: c.node_id.to_vec(),
            endpoint_id: c.endpoint_id.to_vec(),
            owner_instance_id: c.owner_instance_id.to_vec(),
            owner_incarnation: c.owner_incarnation,
            owner_epoch: c.owner_epoch,
            connection_id: c.connection_id.to_vec(),
            authorization_revision: c.authorization_revision,
            capability: c.capability.clone(),
            issued_at: ts(c.issued_at_seconds, c.issued_at_nanos),
            expires_at: ts(c.expires_at_seconds, c.expires_at_nanos),
            signature: hex::decode(s(f, "signature_hex")).expect("sig"),
        };
        (c, m)
    }

    #[test]
    fn complete_config_matches_go_identity_and_approval_vectors() {
        let f = load_fixture("complete-config-v1.json");
        let bytes = |name: &str| hex::decode(s(&f, name)).unwrap();
        let plan =
            ocservia_contracts::decode_strict_command_envelope(&bytes("plan_proto")).unwrap();
        let Some(command_envelope::Payload::CompleteConfigPlan(plan)) = plan.payload else {
            panic!("plan fixture");
        };
        let candidate = plan.candidate.unwrap();
        let canonical = ocservia_contracts::config_profile::canonical(&candidate).unwrap();
        assert_eq!(canonical, bytes("canonical"));
        assert_eq!(
            Sha256::digest(canonical).as_slice(),
            bytes("candidate_hash")
        );
        for kind in ["plan", "apply"] {
            let env = ocservia_contracts::decode_strict_command_envelope(&bytes(&format!(
                "{kind}_proto"
            )))
            .unwrap();
            assert_eq!(
                semantic_payload_hash_v2(&env).unwrap().as_slice(),
                bytes(&format!("{kind}_semantic_hash"))
            );
            assert!(semantic_payload_hash_v1(&env).is_err());
            assert_eq!(
                payload_authorization(&env).unwrap().2,
                format!("ocserv.config.complete.{kind}")
            );
            let mut changed = env;
            changed.node_id[0] ^= 1;
            assert!(semantic_payload_hash_v2(&changed).is_err());
        }
        let mut bad = candidate.clone();
        bad.directives.swap(0, 1);
        assert!(ocservia_contracts::config_profile::canonical(&bad).is_err());
        bad = candidate.clone();
        bad.directives.remove(2);
        assert!(ocservia_contracts::config_profile::canonical(&bad).is_err());
        bad = candidate;
        if let Some(complete_config_directive::Value::Tls(v)) = bad.directives[7].value.as_mut() {
            v.version = "../escape".into();
        }
        assert!(ocservia_contracts::config_profile::canonical(&bad).is_err());

        // Single-admin vs legacy approval binding.
        let mut env =
            ocservia_contracts::decode_strict_command_envelope(&bytes("apply_proto")).unwrap();
        let legacy = semantic_payload_hash_v2(&env).unwrap();
        env.approval_id.clear();
        assert!(semantic_payload_hash_v2(&env).is_err());
        env.approval_request_sha256.clear();
        let direct = semantic_payload_hash_v2(&env).unwrap();
        assert_ne!(direct, legacy);
        env.approval_id = vec![0; 16];
        assert!(semantic_payload_hash_v2(&env).is_err());
        env.approval_id.clear();
        if let Some(command_envelope::Payload::CompleteConfigApply(p)) = &mut env.payload {
            p.materialized_hash[0] ^= 1;
        }
        assert_ne!(semantic_payload_hash_v2(&env).unwrap(), direct);
    }

    #[test]
    fn state_update_operation_identity_matches_the_shared_vector() {
        let node: [u8; 16] = core::array::from_fn(|i| u8::try_from(i + 1).expect("b"));
        let ep: [u8; 32] = core::array::from_fn(|i| u8::try_from(i).expect("b"));
        let id = state_update_operation_id(&node, &ep, 2, 7, "review fixture");
        assert_eq!(hex::encode(id), "f0229ecacf9bb65589e1897c668a48f7");
        assert_ne!(
            state_update_operation_id(&node, &ep, 1, 7, "review fixture"),
            id
        );
        assert_ne!(
            state_update_operation_id(&node, &ep, 2, 8, "review fixture"),
            id
        );
        assert_ne!(
            state_update_operation_id(&node, &ep, 2, 7, "review fixture "),
            id
        );
        let mut other_ep = ep;
        other_ep[0] ^= 1;
        assert_ne!(
            state_update_operation_id(&node, &other_ep, 2, 7, "review fixture"),
            id
        );
    }

    #[test]
    fn rust_verifies_go_command_session_and_artifact_vectors() {
        // Command authorization v1 vectors.
        let f = load_fixture("command-authorization-v1.json");
        assert_eq!(u(&f, "version"), 1);
        let pk = VerifyingKey::from_bytes(&fixed::<32>(s(&f, "public_key_hex"))).expect("pk");
        assert_eq!(verification_key_id(&pk), s(&f, "key_id"));
        let kr = ControllerCommandKeyring::new([pk]).expect("kr");
        for v in f["vectors"].as_array().expect("vectors") {
            let c = CommandAuthorizationV1 {
                authorization_version: 1,
                key_id: s(&f, "key_id").into(),
                protocol_version: s(v, "protocol_version").into(),
                command_id: fixed(s(v, "command_id_hex")),
                idempotency_key: fixed(s(v, "idempotency_key_hex")),
                node_id: fixed(s(v, "node_id_hex")),
                operation_id: fixed(s(v, "operation_id_hex")),
                actor_identity: s(v, "actor_identity").into(),
                action: s(v, "action").into(),
                required_capability: s(v, "required_capability").into(),
                approval_id: opt_fixed(s(v, "approval_id_hex")),
                approval_request_sha256: opt_fixed(s(v, "approval_request_sha256_hex")),
                expected_revision: u(v, "expected_revision"),
                semantic_hash_version: u32v(v, "semantic_hash_version"),
                semantic_payload_sha256: fixed(s(v, "semantic_payload_sha256_hex")),
                payload_kind: u32v(v, "payload_kind"),
                delivery_mode: u32v(v, "delivery_mode"),
                issued_at_seconds: i(v, "issued_at_seconds"),
                issued_at_nanos: u32v(v, "issued_at_nanos"),
                expires_at_seconds: i(v, "expires_at_seconds"),
                expires_at_nanos: u32v(v, "expires_at_nanos"),
            };
            assert_eq!(
                hex::encode(canonical_v1(&c).expect("canon")),
                s(v, "canonical_preimage_hex"),
                "{}",
                s(v, "name")
            );
            let payload = match c.payload_kind {
                104 => command_envelope::Payload::ConfigApply(ConfigApply::default()),
                108 => command_envelope::Payload::SyntheticEcho(SyntheticEcho::default()),
                k => panic!("unsupported payload kind {k}"),
            };
            let env = CommandEnvelope {
                protocol_version: c.protocol_version,
                command_id: c.command_id.to_vec(),
                idempotency_key: c.idempotency_key.to_vec(),
                node_id: c.node_id.to_vec(),
                operation_id: c.operation_id.to_vec(),
                issued_at: ts(c.issued_at_seconds, c.issued_at_nanos),
                expires_at: ts(c.expires_at_seconds, c.expires_at_nanos),
                expected_revision: c.expected_revision,
                actor_id: c.actor_identity,
                action: c.action,
                required_capability: c.required_capability,
                approval_id: c.approval_id.map_or_else(Vec::new, |x| x.to_vec()),
                approval_request_sha256: c
                    .approval_request_sha256
                    .map_or_else(Vec::new, |x| x.to_vec()),
                semantic_payload_hash_version: i32::try_from(c.semantic_hash_version).expect("v"),
                semantic_payload_sha256: c.semantic_payload_sha256.to_vec(),
                delivery_mode: i32::try_from(c.delivery_mode).expect("d"),
                payload: Some(payload),
                authorization: Some(CommandAuthorizationProof {
                    version: CommandAuthorizationVersion::V1.into(),
                    key_id: c.key_id,
                    signature: hex::decode(s(v, "signature_hex")).expect("sig"),
                }),
                ..CommandEnvelope::default()
            };
            kr.verify(&env)
                .unwrap_or_else(|e| panic!("{} Go sig: {e}", s(v, "name")));
        }

        // Session grant v1 vector.
        let sf = load_fixture("session-grant-v1.json");
        let skr = fixture_keyring(&sf);
        let sc = SessionGrantClaimsV1 {
            version: u32v(&sf, "version"),
            key_id: s(&sf, "key_id").into(),
            protocol_major: u32v(&sf, "protocol_major"),
            protocol_minor: u32v(&sf, "protocol_minor"),
            node_id: fixed(s(&sf, "node_id_hex")),
            endpoint_id: fixed(s(&sf, "endpoint_id_hex")),
            authorization_revision: u(&sf, "authorization_revision"),
            negotiated_capabilities: str_vec(&sf, "negotiated_capabilities"),
            issued_at_seconds: i(&sf, "issued_at_seconds"),
            issued_at_nanos: u32v(&sf, "issued_at_nanos"),
            expires_at_seconds: i(&sf, "expires_at_seconds"),
            expires_at_nanos: u32v(&sf, "expires_at_nanos"),
        };
        assert_eq!(
            hex::encode(canonical_session_grant_v1(&sc).expect("sg")),
            s(&sf, "canonical_preimage_hex")
        );
        let mut sg = SessionGrantV1 {
            version: SessionGrantVersion::V1.into(),
            key_id: sc.key_id.clone(),
            protocol_major: sc.protocol_major,
            protocol_minor: sc.protocol_minor,
            node_id: sc.node_id.to_vec(),
            endpoint_id: sc.endpoint_id.to_vec(),
            authorization_revision: sc.authorization_revision,
            negotiated_capabilities: sc.negotiated_capabilities.clone(),
            issued_at: ts(sc.issued_at_seconds, sc.issued_at_nanos),
            expires_at: ts(sc.expires_at_seconds, sc.expires_at_nanos),
            signature: hex::decode(s(&sf, "signature_hex")).expect("sig"),
        };
        assert_eq!(
            skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds)
                .expect("sg")
                .authorization_revision,
            sc.authorization_revision
        );
        assert_eq!(
            skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.expires_at_seconds),
            Err(AuthorizationError::ClaimsInvalid("session_grant_expired"))
        );
        let mut bad_sg = sg.clone();
        bad_sg.key_id = "ed25519-sha256:unknown".into();
        assert_eq!(
            skr.verify_session_grant(&bad_sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds),
            Err(AuthorizationError::UnknownKey)
        );
        sg.authorization_revision += 1;
        assert_eq!(
            skr.verify_session_grant(&sg, &sc.node_id, &sc.endpoint_id, sc.issued_at_seconds),
            Err(AuthorizationError::SignatureInvalid)
        );

        // Artifact grant v1 vector.
        let af = load_fixture("artifact-grant-v1.json");
        let akr = fixture_keyring(&af);
        let ac = ArtifactGrantClaimsV1 {
            version: u32v(&af, "version"),
            key_id: s(&af, "key_id").into(),
            node_id: fixed(s(&af, "node_id_hex")),
            artifact_id: fixed(s(&af, "artifact_id_hex")),
            certificate_id: fixed(s(&af, "certificate_id_hex")),
            certificate_version: u(&af, "certificate_version"),
            operation_id: fixed(s(&af, "operation_id_hex")),
            authorized_subject: s(&af, "authorized_subject").into(),
            purpose: s(&af, "purpose").into(),
            max_bytes: u(&af, "max_bytes"),
            issued_at_seconds: i(&af, "issued_at_seconds"),
            issued_at_nanos: u32v(&af, "issued_at_nanos"),
            expires_at_seconds: i(&af, "expires_at_seconds"),
            expires_at_nanos: u32v(&af, "expires_at_nanos"),
            grant_id: fixed(s(&af, "grant_id_hex")),
        };
        assert_eq!(
            hex::encode(canonical_artifact_grant_v1(&ac).expect("ag")),
            s(&af, "canonical_preimage_hex")
        );
        let ag = ArtifactGrantV1 {
            version: ArtifactGrantVersion::V1.into(),
            key_id: ac.key_id.clone(),
            node_id: ac.node_id.to_vec(),
            artifact_id: ac.artifact_id.to_vec(),
            certificate_id: ac.certificate_id.to_vec(),
            certificate_version: ac.certificate_version,
            operation_id: ac.operation_id.to_vec(),
            authorized_subject: ac.authorized_subject.clone(),
            purpose: ac.purpose.clone(),
            max_bytes: ac.max_bytes,
            issued_at: ts(ac.issued_at_seconds, ac.issued_at_nanos),
            expires_at: ts(ac.expires_at_seconds, ac.expires_at_nanos),
            grant_id: ac.grant_id.to_vec(),
            signature: hex::decode(s(&af, "signature_hex")).expect("sig"),
        };
        akr.verify_artifact_grant(
            &ag,
            &ac.node_id,
            &ac.artifact_id,
            &ac.purpose,
            ac.max_bytes,
            ac.issued_at_seconds,
        )
        .expect("ag");
        assert_eq!(
            akr.verify_artifact_grant(
                &ag,
                &ac.node_id,
                &ac.artifact_id,
                &ac.purpose,
                ac.max_bytes,
                ac.expires_at_seconds
            ),
            Err(AuthorizationError::ClaimsInvalid("artifact_grant_expired"))
        );
        akr.verify_artifact_grant_for_confirmation(
            &ag,
            &ac.node_id,
            &ac.artifact_id,
            &ac.purpose,
            ac.max_bytes,
            ac.expires_at_seconds,
        )
        .expect("confirm");
        let mut forged = ag.clone();
        forged.signature[0] ^= 1;
        assert_eq!(
            akr.verify_artifact_grant_for_confirmation(
                &forged,
                &ac.node_id,
                &ac.artifact_id,
                &ac.purpose,
                ac.max_bytes,
                ac.expires_at_seconds
            ),
            Err(AuthorizationError::SignatureInvalid)
        );
        let mut unk = ag.clone();
        unk.key_id = "ed25519-sha256:unknown".into();
        assert_eq!(
            akr.verify_artifact_grant(
                &unk,
                &ac.node_id,
                &ac.artifact_id,
                &ac.purpose,
                ac.max_bytes,
                ac.issued_at_seconds
            ),
            Err(AuthorizationError::UnknownKey)
        );
        let mut wrong_node = ac.node_id;
        wrong_node[0] ^= 1;
        assert_eq!(
            akr.verify_artifact_grant(
                &ag,
                &wrong_node,
                &ac.artifact_id,
                &ac.purpose,
                ac.max_bytes,
                ac.issued_at_seconds
            ),
            Err(AuthorizationError::ClaimsInvalid(
                "artifact_grant_node_mismatch"
            ))
        );
        let mutations: [ArtifactMutation; 4] = [
            ("cert_ver", |v| v.certificate_version += 1),
            ("op", |v| v.operation_id[0] ^= 1),
            ("grant_id", |v| v.grant_id[0] ^= 1),
            ("sig", |v| v.signature[0] ^= 1),
        ];
        for (name, m) in mutations {
            let mut mod_g = ag.clone();
            m(&mut mod_g);
            assert_eq!(
                akr.verify_artifact_grant(
                    &mod_g,
                    &ac.node_id,
                    &ac.artifact_id,
                    &ac.purpose,
                    ac.max_bytes,
                    ac.issued_at_seconds
                ),
                Err(AuthorizationError::SignatureInvalid),
                "{name}"
            );
        }
    }

    #[test]
    fn rust_verifies_go_fence_v2_binding_nanosecond_bounds_and_domain_separation() {
        let ff = load_fixture("connection-fence-v2.json");
        let kr = fixture_keyring(&ff);
        let (fc, fence) = fence_from_fixture(&ff);
        let fcanon = canonical_connection_fence_v2(&fc).expect("canon fence");
        assert_eq!(hex::encode(&fcanon), s(&ff, "canonical_preimage_hex"));
        let vf = kr
            .verify_connection_fence_v2(&fence, &fc.node_id, &fc.endpoint_id, 1_700_000_100)
            .expect("vf");
        assert_eq!(
            (
                vf.fence_id,
                vf.owner_instance_id,
                vf.owner_incarnation,
                vf.owner_epoch,
                vf.connection_id,
                vf.authorization_revision,
                vf.lease_until_seconds
            ),
            (
                fc.fence_id,
                fc.owner_instance_id,
                fc.owner_incarnation,
                fc.owner_epoch,
                fc.connection_id,
                fc.authorization_revision,
                fc.lease_until_seconds
            )
        );
        assert_eq!(vf.capabilities, fc.capabilities);
        assert_eq!(
            kr.verify_connection_fence_v2(&fence, &[9; 16], &fc.endpoint_id, 1_700_000_100),
            Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_node_mismatch"
            ))
        );
        assert_eq!(
            kr.verify_connection_fence_v2(&fence, &fc.node_id, &[9; 32], 1_700_000_100),
            Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_endpoint_mismatch"
            ))
        );
        let mut tamp = fence.clone();
        tamp.owner_epoch += 1;
        assert_eq!(
            kr.verify_connection_fence_v2(&tamp, &fc.node_id, &fc.endpoint_id, 1_700_000_100),
            Err(AuthorizationError::SignatureInvalid)
        );
        assert_eq!(
            kr.verify_connection_fence_v2(&fence, &fc.node_id, &fc.endpoint_id, 1_700_000_301),
            Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_expired"
            ))
        );

        // Nanosecond bounds on fence v2.
        let skew = fc
            .issued_at_seconds
            .checked_sub(MAX_FUTURE_SKEW_SECONDS)
            .expect("skew");
        kr.verify_connection_fence_v2_at(
            &fence,
            &fc.node_id,
            &fc.endpoint_id,
            fc.expires_at_seconds,
            fc.expires_at_nanos - 1,
        )
        .expect("1ns before");
        assert_eq!(
            kr.verify_connection_fence_v2_at(
                &fence,
                &fc.node_id,
                &fc.endpoint_id,
                fc.expires_at_seconds,
                fc.expires_at_nanos
            ),
            Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_expired"
            ))
        );
        kr.verify_connection_fence_v2_at(
            &fence,
            &fc.node_id,
            &fc.endpoint_id,
            skew,
            fc.issued_at_nanos,
        )
        .expect("exact skew");
        assert_eq!(
            kr.verify_connection_fence_v2_at(
                &fence,
                &fc.node_id,
                &fc.endpoint_id,
                skew,
                fc.issued_at_nanos - 1
            ),
            Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_clock_skew"
            ))
        );
        kr.verify_connection_fence_v2(&fence, &fc.node_id, &fc.endpoint_id, fc.expires_at_seconds)
            .expect("sec-only at ns=0");

        // Domain separation vs V1.
        let signing = SigningKey::from_bytes(&fixed::<32>(s(&ff, "test_seed_hex")));
        let sig = signing.sign(&fcanon).to_bytes();
        for v1 in [
            DOMAIN_SEPARATOR,
            SESSION_GRANT_DOMAIN_SEPARATOR,
            ARTIFACT_GRANT_DOMAIN_SEPARATOR,
        ] {
            let mut forged = v1.to_vec();
            forged.extend_from_slice(&fcanon[CONNECTION_FENCE_V2_DOMAIN_SEPARATOR.len()..]);
            assert_ne!(forged, fcanon);
            assert_ne!(signing.sign(&forged).to_bytes(), sig);
        }
        let mut cross = fence;
        let mut v1_body = SESSION_GRANT_DOMAIN_SEPARATOR.to_vec();
        v1_body.extend_from_slice(&fcanon[CONNECTION_FENCE_V2_DOMAIN_SEPARATOR.len()..]);
        cross.signature = signing.sign(&v1_body).to_bytes().to_vec();
        assert_eq!(
            kr.verify_connection_fence_v2(&cross, &fc.node_id, &fc.endpoint_id, 1_700_000_100),
            Err(AuthorizationError::SignatureInvalid)
        );

        // Fence binding v2 vector + term matching + nanosecond bounds.
        let bf = load_fixture("fence-binding-v2.json");
        let bkr = fixture_keyring(&bf);
        let (bc, binding) = binding_from_fixture(&bf);
        assert_eq!(
            hex::encode(canonical_fence_binding_v2(&bc).expect("canon binding")),
            s(&bf, "canonical_preimage_hex")
        );
        let vb = bkr
            .verify_fence_binding_v2(&binding, &bc.node_id, &bc.endpoint_id, 1_700_000_100)
            .expect("vb");
        assert!(vb.matches_fence(&vf));
        for mutate in [
            (|f: &mut VerifiedConnectionFenceV2| f.owner_epoch += 1)
                as fn(&mut VerifiedConnectionFenceV2),
            |f| f.connection_id = [9; 16],
            |f| f.fence_id = [9; 16],
        ] {
            let mut drifted = vf.clone();
            mutate(&mut drifted);
            assert!(!vb.matches_fence(&drifted));
        }
        let mut tb = binding.clone();
        tb.operation_id = vec![9; 16];
        assert_eq!(
            bkr.verify_fence_binding_v2(&tb, &bc.node_id, &bc.endpoint_id, 1_700_000_100),
            Err(AuthorizationError::SignatureInvalid)
        );
        let mut dg = binding.clone();
        dg.signature_version = FenceSignatureVersion::Unspecified.into();
        assert_eq!(
            bkr.verify_fence_binding_v2(&dg, &bc.node_id, &bc.endpoint_id, 1_700_000_100),
            Err(AuthorizationError::UnsupportedVersion)
        );

        let bskew = bc
            .issued_at_seconds
            .checked_sub(MAX_FUTURE_SKEW_SECONDS)
            .expect("bskew");
        bkr.verify_fence_binding_v2_at(
            &binding,
            &bc.node_id,
            &bc.endpoint_id,
            bc.expires_at_seconds,
            bc.expires_at_nanos - 1,
        )
        .expect("1ns");
        assert_eq!(
            bkr.verify_fence_binding_v2_at(
                &binding,
                &bc.node_id,
                &bc.endpoint_id,
                bc.expires_at_seconds,
                bc.expires_at_nanos
            ),
            Err(AuthorizationError::ClaimsInvalid("fence_binding_expired"))
        );
        bkr.verify_fence_binding_v2_at(
            &binding,
            &bc.node_id,
            &bc.endpoint_id,
            bskew,
            bc.issued_at_nanos,
        )
        .expect("exact");
        assert_eq!(
            bkr.verify_fence_binding_v2_at(
                &binding,
                &bc.node_id,
                &bc.endpoint_id,
                bskew,
                bc.issued_at_nanos - 1
            ),
            Err(AuthorizationError::ClaimsInvalid(
                "fence_binding_clock_skew"
            ))
        );
        bkr.verify_fence_binding_v2(
            &binding,
            &bc.node_id,
            &bc.endpoint_id,
            bc.expires_at_seconds,
        )
        .expect("sec-only");
    }

    #[test]
    fn verification_key_loader_and_shared_metadata_policy() {
        let suffix = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let dir = std::env::current_dir()
            .expect("cwd")
            .join(format!(".command-key-test-{}-{suffix}", std::process::id()));
        fs::create_dir(&dir).expect("mkdir");
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).expect("700");
        let key_path = dir.join("controller.pem");
        let key = SigningKey::from_bytes(&[7; 32]).verifying_key();
        fs::write(
            &key_path,
            key.to_public_key_pem(LineEnding::LF).expect("pem"),
        )
        .expect("write");
        fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).expect("600");
        let (uid, gid) = (
            rustix::process::geteuid().as_raw(),
            rustix::process::getegid().as_raw(),
        );
        assert_eq!(
            load_verification_key(&key_path, uid, gid).expect("safe"),
            key
        );

        fs::set_permissions(&key_path, fs::Permissions::from_mode(0o644)).expect("644");
        assert!(load_verification_key(&key_path, uid, gid).is_err());
        fs::set_permissions(&key_path, fs::Permissions::from_mode(0o600)).expect("600");
        let link = dir.join("controller-link.pem");
        symlink(&key_path, &link).expect("symlink");
        assert!(load_verification_key(&link, uid, gid).is_err());
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o770)).expect("770");
        assert!(load_verification_key(&key_path, uid, gid).is_err());
        let _ = fs::remove_dir_all(&dir);

        let (agent_user_id, agent_group_id) = (997, 998);
        for mode in [0o440, 0o640] {
            assert!(verification_key_metadata_allowed(
                0,
                agent_group_id,
                mode,
                agent_user_id,
                agent_group_id
            ));
            assert!(verification_key_metadata_allowed(
                0,
                agent_group_id,
                mode,
                0,
                agent_group_id
            ));
        }
        for mode in [0o400, 0o600] {
            assert!(verification_key_metadata_allowed(
                agent_user_id,
                agent_group_id,
                mode,
                agent_user_id,
                agent_group_id
            ));
            assert!(!verification_key_metadata_allowed(
                agent_user_id,
                agent_group_id,
                mode,
                0,
                agent_group_id
            ));
            assert!(!verification_key_metadata_allowed(
                0,
                agent_group_id,
                mode,
                agent_user_id,
                agent_group_id
            ));
            assert!(verification_key_metadata_allowed(
                0,
                agent_group_id,
                mode,
                0,
                agent_group_id
            ));
        }
        assert!(!verification_key_metadata_allowed(
            0,
            agent_group_id + 1,
            0o640,
            agent_user_id,
            agent_group_id
        ));
        assert!(!verification_key_metadata_allowed(
            0,
            agent_group_id + 1,
            0o640,
            0,
            agent_group_id
        ));
    }

    #[test]
    fn canonical_semantic_hash_v2_matches_shared_agent_upgrade_vector() {
        let f = load_fixture("semantic-payload-hash-v2-agent-upgrade.json");
        let v = &f["vector"];
        assert_eq!(u(v, "payload_kind"), 128);
        let env = CommandEnvelope {
            node_id: hex::decode(s(v, "node_id_hex")).expect("node"),
            expected_revision: u(v, "authorization_revision"),
            payload: Some(command_envelope::Payload::AgentUpgrade(AgentUpgrade {
                target_version: s(v, "target_version").into(),
                package_sha256: hex::decode(s(v, "package_sha256_hex")).expect("sha"),
                architecture: s(v, "architecture").into(),
            })),
            ..CommandEnvelope::default()
        };
        let digest = semantic_payload_hash_v2(&env).expect("v2");
        assert_eq!(hex::encode(digest), s(v, "expected_sha256"));
        for (name, val, reject) in [
            ("ver", "1.2.4", false),
            ("arch", "amd64", false),
            ("ver", "latest", true),
            ("ver", "1.2", true),
        ] {
            let mut c = env.clone();
            if let Some(command_envelope::Payload::AgentUpgrade(p)) = c.payload.as_mut() {
                if name == "ver" {
                    p.target_version = val.into();
                } else {
                    p.architecture = val.into();
                }
            }
            if reject {
                assert_eq!(
                    semantic_payload_hash_v2(&c),
                    Err(AuthorizationError::ClaimsInvalid("agent_upgrade_invalid"))
                );
            } else {
                assert_ne!(semantic_payload_hash_v2(&c).expect("ok"), digest);
            }
        }
        let mut short = env;
        if let Some(command_envelope::Payload::AgentUpgrade(p)) = short.payload.as_mut() {
            p.package_sha256.pop();
        }
        assert_eq!(
            semantic_payload_hash_v2(&short),
            Err(AuthorizationError::ClaimsInvalid("agent_upgrade_invalid"))
        );
    }
}
