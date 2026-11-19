use std::fs::File;
use std::io::{self, Read as _};
use std::os::unix::fs::{MetadataExt as _, PermissionsExt as _};
use std::path::{Component, Path};
use std::sync::Arc;

use ed25519_dalek::pkcs8::DecodePublicKey as _;
use ed25519_dalek::{Signature, VerifyingKey};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, ArtifactGrantVersion, CommandAuthorizationVersion, CommandDeliveryMode,
    CommandEnvelope, ConnectionFenceV2, FenceBindingV2, SessionGrantV1, SessionGrantVersion,
};
use rustix::fs::{Mode, OFlags};

use crate::fence::{
    FenceBindingClaimsV2, VerifiedConnectionFenceV2, canonical_connection_fence_v2,
    canonical_fence_binding_v2, connection_fence_claims_v2, fence_binding_claims_v2,
    timestamp_exceeds_future_skew, timestamp_expired,
};
use crate::semantic_hash::verify_semantic_payload_hash;
use crate::{
    ArtifactGrantClaimsV1, AuthorizationError, CommandAuthorizationV1, MAX_FUTURE_SKEW_SECONDS,
    VerifiedSessionGrant, artifact_grant_claims_v1, canonical_artifact_grant_v1,
    canonical_session_grant_v1, canonical_v1, claims_from_envelope_v1, session_grant_claims_v1,
    verification_key_id,
};

/// A bounded set of Controller public keys pinned by the Agent.
///
/// The rotation set is bounded to at most eight keys, so an immutable
/// `Arc<[(String, VerifyingKey)]>` gives O(1) refcount cloning across sessions
/// and cache-friendly lookups without hashing overhead.
#[derive(Clone, Debug)]
pub struct ControllerCommandKeyring {
    keys: Arc<[(String, VerifyingKey)]>,
}

impl ControllerCommandKeyring {
    /// Creates a keyring from one or more distinct pinned keys.
    ///
    /// # Errors
    ///
    /// Rejects an empty key list, more than eight rotation keys, or duplicate IDs.
    pub fn new(keys: impl IntoIterator<Item = VerifyingKey>) -> Result<Self, AuthorizationError> {
        let mut values: Vec<(String, VerifyingKey)> = Vec::with_capacity(8);
        for key in keys {
            let key_id = verification_key_id(&key);
            if values.iter().any(|(existing, _)| existing == &key_id) {
                return Err(AuthorizationError::ClaimsInvalid(
                    "command_authorization_key_duplicate",
                ));
            }
            if values.len() >= 8 {
                return Err(AuthorizationError::ClaimsInvalid(
                    "command_authorization_key_count_invalid",
                ));
            }
            values.push((key_id, key));
        }
        if values.is_empty() {
            return Err(AuthorizationError::ClaimsInvalid(
                "command_authorization_key_missing",
            ));
        }
        Ok(Self {
            keys: Arc::from(values),
        })
    }

    fn key(&self, key_id: &str) -> Option<&VerifyingKey> {
        self.keys
            .iter()
            .find_map(|(existing, key)| (existing == key_id).then_some(key))
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
            .key(&proof.key_id)
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
            .key(&grant.key_id)
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
            .key(&claims.key_id)
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
            .key(&claims.key_id)
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
            .key(&grant.key_id)
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

pub(crate) const fn verification_key_metadata_allowed(
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

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::PermissionDenied, message)
}
