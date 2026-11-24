use std::fs::File;
use std::io::{self, Read as _};
use std::os::unix::fs::{MetadataExt as _, PermissionsExt as _};
use std::path::{Component, Path};
use std::sync::Arc;

use ed25519_dalek::pkcs8::DecodePublicKey as _;
use ed25519_dalek::{Signature, VerifyingKey};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactGrantV1, CommandDeliveryMode, CommandEnvelope, SessionGrantV1,
};
use rustix::fs::{Mode, OFlags};

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

/// A signed command whose semantic payload and local node binding were verified.
#[derive(Debug)]
pub struct VerifiedCommand<'a> {
    envelope: &'a CommandEnvelope,
    claims: CommandAuthorizationV1,
}

impl<'a> VerifiedCommand<'a> {
    #[must_use]
    pub const fn envelope(&self) -> &'a CommandEnvelope {
        self.envelope
    }

    #[must_use]
    pub const fn claims(&self) -> &CommandAuthorizationV1 {
        &self.claims
    }
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
        if proof.version != 1 {
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
    pub fn verify_command<'a>(
        &self,
        envelope: &'a CommandEnvelope,
        expected_node_id: &[u8; 16],
        now_unix_seconds: i64,
    ) -> Result<VerifiedCommand<'a>, AuthorizationError> {
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
        Ok(VerifiedCommand { envelope, claims })
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
        if grant.version != 1 {
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
            session_epoch: claims.session_epoch,
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
        if grant.version != 1 {
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
