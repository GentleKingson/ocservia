//! AF_UNIX-only privileged helper exposing fixed, typed Ocserv operations.

#![forbid(unsafe_code)]

use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ed25519_dalek::SigningKey;
use ocservia_agent_protocol::{
    AgentUpgradeScheduledResult, ErrorKind, PrivdError, PrivdRequest, PrivdResponse,
    PrivilegedRequestMode, UpgradeOperationResult, UpgradeResultList, privd_request,
    privd_response, read_frame, write_frame,
};
use ocservia_command_authorization::{
    ArtifactGrantClaimsV1, AuthorizationError, CommandAuthorizationV1, ControllerCommandKeyring,
    FenceBindingClaimsV2, VerifiedConnectionFenceV2,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, AgentUpgradeOutcomeState, AgentUpgradeResultProof, CommandDeliveryMode,
    CommandEnvelope, CommandResultState, ConnectionFenceV2, FenceBindingV2, FenceOperationKind,
    PrivdCertificateReceiptBindingV1, PrivdReceiptVersion, PrivdResultReceiptV1,
    PrivilegedCommandKind, PrivilegedResultKind, SealedSecretPurpose, SealedSecretVersion,
    command_envelope,
};
use ocservia_ocserv_adapter::{
    Adapter, ArtifactLeaseIdentity, AuthorizedEffectDecision, AuthorizedEffectIdentity,
    EffectIdentity,
};
use ocservia_privd_attestation::{
    key_id, requested_subject_digest, sign_receipt, sign_upgrade_result,
};
use ocservia_upgrader::{ScheduleOutcome, UpgradeIntent, UpgradeScheduler, UpgradeStoreError};
use prost::Message as _;
use sha2::{Digest as _, Sha256};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::Semaphore;
use uuid::Uuid;

const MAX_CONCURRENT_CLIENTS: usize = 4;
const MAX_REQUEST_LIFETIME: Duration = Duration::from_secs(30);
/// Capability every Controller artifact fence binding names (same as the Agent).
const CONNECTION_FENCING_CAPABILITY: &str = "ocserv.fencing.v2";

/// Privd server configuration.
#[derive(Clone)]
pub struct ServerConfig {
    /// `AF_UNIX` socket path.
    pub socket: PathBuf,
    /// Only this peer UID may issue requests.
    pub agent_uid: u32,
    /// Local node UUID pinned independently from the Agent request.
    pub node_id: [u8; 16],
    /// Enrolled Agent Iroh `EndpointID` pinned by root, never taken from the
    /// Agent request; signed connection fences must name it.
    pub endpoint_id: [u8; 32],
    /// Controller verification keys loaded by root at startup.
    pub command_keys: ControllerCommandKeyring,
    /// Root-owned per-node terminal-result attestation key.
    pub attestation_key: Arc<SigningKey>,
    /// Durable self-upgrade scheduling boundary; privd commits intents and
    /// starts the fixed upgrader unit but never executes an upgrade.
    pub upgrades: UpgradeScheduler,
}

impl std::fmt::Debug for ServerConfig {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ServerConfig")
            .field("socket", &self.socket)
            .field("agent_uid", &self.agent_uid)
            .field("node_id", &Uuid::from_bytes(self.node_id))
            .field("endpoint_id", &hex::encode(self.endpoint_id))
            .field("attestation_key", &"[redacted]")
            .field("upgrades", &self.upgrades)
            .finish_non_exhaustive()
    }
}

/// Binds a root-controlled `AF_UNIX` socket with mode 0660.
///
/// # Errors
///
/// Refuses insecure directories and non-socket path replacement.
pub fn bind_socket(config: &ServerConfig) -> Result<UnixListener, io::Error> {
    let parent = config.socket.parent().ok_or_else(|| {
        io::Error::new(io::ErrorKind::InvalidInput, "privd socket needs a parent")
    })?;
    let parent_metadata = fs::symlink_metadata(parent)?;
    if !parent_metadata.is_dir()
        || parent_metadata.file_type().is_symlink()
        || parent_metadata.uid() != rustix_uid()
        || parent_metadata.mode() & 0o022 != 0
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "privd socket directory must not be attacker-writable",
        ));
    }
    match fs::symlink_metadata(&config.socket) {
        Ok(metadata) if metadata.file_type().is_socket() && metadata.uid() == rustix_uid() => {
            fs::remove_file(&config.socket)?;
        }
        Ok(_) => {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "privd socket path replacement refused",
            ));
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let listener = UnixListener::bind(&config.socket)?;
    fs::set_permissions(&config.socket, fs::Permissions::from_mode(0o660))?;
    Ok(listener)
}

#[cfg(target_family = "unix")]
fn rustix_uid() -> u32 {
    rustix::process::geteuid().as_raw()
}

/// Serves requests until shutdown.
///
/// # Errors
///
/// Returns an accept-loop error. Individual invalid clients are logged and dropped.
pub async fn serve(
    listener: UnixListener,
    config: ServerConfig,
    adapter: Adapter,
    shutdown: impl std::future::Future<Output = ()>,
) -> Result<(), io::Error> {
    let permits = Arc::new(Semaphore::new(MAX_CONCURRENT_CLIENTS));
    tokio::pin!(shutdown);
    loop {
        tokio::select! {
            () = &mut shutdown => return Ok(()),
            accepted = listener.accept() => {
                let (stream, _) = accepted?;
                // Keep execution at the fixed concurrency cap, but queue at
                // most this one already-accepted local client while a prior
                // request releases its permit. Dropping it here races the
                // Agent's four-request snapshot batches and can tear down an
                // otherwise healthy authoritative Controller session.
                let permit = tokio::select! {
                    () = &mut shutdown => return Ok(()),
                    permit = Arc::clone(&permits).acquire_owned() => permit
                        .map_err(|_| io::Error::other("privd concurrency limiter closed"))?,
                };
                let adapter = adapter.clone();
                let agent_uid = config.agent_uid;
                let node_id = config.node_id;
                let endpoint_id = config.endpoint_id;
                let command_keys = config.command_keys.clone();
                let attestation_key = Arc::clone(&config.attestation_key);
                let upgrades = config.upgrades.clone();
                tokio::spawn(async move {
                    let _permit = permit;
                    if let Err(error) = handle_client(
                        stream,
                        agent_uid,
                        node_id,
                        endpoint_id,
                        command_keys,
                        attestation_key,
                        upgrades,
                        adapter,
                    )
                    .await
                    {
                        tracing::warn!(error = %error, "privd client failed");
                    }
                });
            }
        }
    }
}

#[allow(clippy::too_many_arguments)]
async fn handle_client(
    mut stream: UnixStream,
    agent_uid: u32,
    node_id: [u8; 16],
    endpoint_id: [u8; 32],
    command_keys: ControllerCommandKeyring,
    attestation_key: Arc<SigningKey>,
    upgrades: UpgradeScheduler,
    adapter: Adapter,
) -> Result<(), io::Error> {
    let credentials = stream.peer_cred()?;
    if credentials.uid() != agent_uid {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "privd peer UID refused",
        ));
    }
    let request: PrivdRequest = tokio::time::timeout(MAX_REQUEST_LIFETIME, read_frame(&mut stream))
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd request read timed out"))??;
    let response = dispatch_attested(
        &request,
        &node_id,
        &endpoint_id,
        &command_keys,
        &attestation_key,
        &upgrades,
        &adapter,
    )
    .await;
    tokio::time::timeout(MAX_REQUEST_LIFETIME, write_frame(&mut stream, &response))
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd response write timed out"))??;
    Ok(())
}

async fn dispatch_attested(
    request: &PrivdRequest,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    attestation_key: &SigningKey,
    upgrades: &UpgradeScheduler,
    adapter: &Adapter,
) -> PrivdResponse {
    let request_id = request.request_id.clone();
    let result = match validate_request(request, node_id, endpoint_id, command_keys) {
        Ok((deadline, ValidatedRequest::Read)) => {
            match tokio::time::timeout(
                deadline,
                execute_read(
                    request.operation.clone(),
                    node_id,
                    attestation_key,
                    adapter,
                    upgrades,
                ),
            )
            .await
            {
                Ok(result) => result,
                Err(_) => deadline_error(),
            }
        }
        Ok((deadline, ValidatedRequest::Execute(command, claims, accepted_at))) => {
            match tokio::time::timeout(
                deadline,
                execute_command(
                    command,
                    &claims,
                    &accepted_at,
                    node_id,
                    attestation_key,
                    upgrades,
                    adapter,
                ),
            )
            .await
            {
                Ok(mut response) => {
                    response.request_id = request_id;
                    return response;
                }
                Err(_) => deadline_error(),
            }
        }
        Ok((deadline, ValidatedRequest::Reconcile(command, claims))) => {
            match tokio::time::timeout(deadline, reconcile_command(command, &claims, adapter)).await
            {
                Ok(mut response) => {
                    response.request_id = request_id;
                    return response;
                }
                Err(_) => deadline_error(),
            }
        }
        Ok((deadline, ValidatedRequest::ArtifactRead(claims, offset))) => {
            match tokio::time::timeout(
                deadline,
                adapter.artifact_read(artifact_identity(&claims), offset),
            )
            .await
            {
                Ok(result) => finish_operation(
                    "artifact_read",
                    result.map(privd_response::Result::ArtifactData),
                ),
                Err(_) => deadline_error(),
            }
        }
        Ok((deadline, ValidatedRequest::ArtifactConsume(claims, digest, size, confirm_only))) => {
            match tokio::time::timeout(deadline, async {
                if confirm_only {
                    adapter
                        .artifact_confirm_consumed(artifact_identity(&claims), &digest, size)
                        .await
                } else {
                    adapter
                        .artifact_consume(artifact_identity(&claims), &digest, size)
                        .await
                }
            })
            .await
            {
                Ok(result) => finish_operation(
                    "artifact_consume",
                    result.map(privd_response::Result::Mutation),
                ),
                Err(_) => deadline_error(),
            }
        }
        Err(failure) => privd_response::Result::Error(failure),
    };
    PrivdResponse {
        request_id,
        privileged_result_proof: None,
        result: Some(result),
    }
}

#[cfg(test)]
const TEST_ENDPOINT_ID: [u8; 32] = [9; 32];

#[cfg(test)]
async fn dispatch(
    request: PrivdRequest,
    node_id: &[u8; 16],
    command_keys: &ControllerCommandKeyring,
    adapter: &Adapter,
) -> PrivdResponse {
    let key = SigningKey::from_bytes(&[41; 32]);
    dispatch_attested(
        &request,
        node_id,
        &TEST_ENDPOINT_ID,
        command_keys,
        &key,
        &UpgradeScheduler::disabled(),
        adapter,
    )
    .await
}

#[cfg(test)]
async fn dispatch_upgrade(
    request: PrivdRequest,
    node_id: &[u8; 16],
    command_keys: &ControllerCommandKeyring,
    adapter: &Adapter,
    upgrades: &UpgradeScheduler,
) -> PrivdResponse {
    let key = SigningKey::from_bytes(&[41; 32]);
    dispatch_attested(
        &request,
        node_id,
        &TEST_ENDPOINT_ID,
        command_keys,
        &key,
        upgrades,
        adapter,
    )
    .await
}

enum ValidatedRequest<'a> {
    Read,
    Execute(
        &'a CommandEnvelope,
        CommandAuthorizationV1,
        prost_types::Timestamp,
    ),
    Reconcile(&'a CommandEnvelope, CommandAuthorizationV1),
    ArtifactRead(ArtifactGrantClaimsV1, u64),
    ArtifactConsume(ArtifactGrantClaimsV1, Vec<u8>, u64, bool),
}

#[allow(clippy::too_many_lines)]
fn validate_request<'a>(
    request: &'a PrivdRequest,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
) -> Result<(Duration, ValidatedRequest<'a>), PrivdError> {
    let id = Uuid::from_slice(&request.request_id)
        .map_err(|_| error(ErrorKind::InvalidRequest, "request_id must be UUIDv7"))?;
    if id.get_version_num() != 7 {
        return Err(error(
            ErrorKind::InvalidRequest,
            "request_id must be UUIDv7",
        ));
    }
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
    let now_ms = u64::try_from(now.as_millis())
        .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
    if request.deadline_unix_ms <= now_ms {
        return Err(error(
            ErrorKind::DeadlineExceeded,
            "request deadline exceeded",
        ));
    }
    let remaining = Duration::from_millis(request.deadline_unix_ms - now_ms);
    if remaining > MAX_REQUEST_LIFETIME {
        return Err(error(
            ErrorKind::InvalidRequest,
            "request deadline too far in future",
        ));
    }
    let mode = PrivilegedRequestMode::try_from(request.privileged_mode)
        .unwrap_or(PrivilegedRequestMode::Unspecified);
    let validated = if let Some(command) = request.authorization_command.as_ref() {
        if request.operation.is_some() || mode == PrivilegedRequestMode::Unspecified {
            return Err(error(
                ErrorKind::InvalidRequest,
                "signed command request shape invalid",
            ));
        }
        let now_seconds = i64::try_from(now.as_secs())
            .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
        let claims = command_keys
            .verify_command(command, node_id, now_seconds)
            .map_err(|failure| authorization_error(&failure))?;
        validate_privileged_payload(command, &claims)?;
        let delivery = CommandDeliveryMode::try_from(command.delivery_mode)
            .unwrap_or(CommandDeliveryMode::Unspecified);
        match (mode, delivery) {
            (
                PrivilegedRequestMode::Execute,
                CommandDeliveryMode::ExecuteOrReplay | CommandDeliveryMode::RetryIfEffectAbsent,
            ) => {
                let accepted = request.accepted_at.ok_or_else(|| {
                    error(
                        ErrorKind::InvalidRequest,
                        "journal acceptance timestamp required",
                    )
                })?;
                if accepted.seconds < claims.issued_at_seconds
                    || accepted.seconds > now_seconds.saturating_add(300)
                    || !(0..1_000_000_000).contains(&accepted.nanos)
                {
                    return Err(error(
                        ErrorKind::InvalidRequest,
                        "journal acceptance timestamp invalid",
                    ));
                }
                verify_execution_fence(
                    command,
                    &claims,
                    node_id,
                    endpoint_id,
                    command_keys,
                    now_seconds,
                    now.subsec_nanos(),
                )?;
                ValidatedRequest::Execute(command, claims, accepted)
            }
            (PrivilegedRequestMode::Reconcile, CommandDeliveryMode::ReconcileOnly) => {
                if effect_binding(command, &claims).is_none() {
                    return Err(error(
                        ErrorKind::InvalidRequest,
                        "command does not support privileged reconciliation",
                    ));
                }
                ValidatedRequest::Reconcile(command, claims)
            }
            _ => {
                return Err(error(
                    ErrorKind::PermissionDenied,
                    "signed command delivery mode mismatch",
                ));
            }
        }
    } else {
        if request.accepted_at.is_some() {
            return Err(error(
                ErrorKind::InvalidRequest,
                "journal acceptance timestamp is only valid for execution",
            ));
        }
        if mode != PrivilegedRequestMode::Unspecified {
            return Err(error(
                ErrorKind::PermissionDenied,
                "Controller command authorization required",
            ));
        }
        if let Some(privd_request::Operation::ArtifactRead(value)) = request.operation.as_ref() {
            let grant = value
                .grant
                .as_ref()
                .ok_or_else(|| error(ErrorKind::PermissionDenied, "artifact grant required"))?;
            let artifact_id: [u8; 16] = grant
                .artifact_id
                .as_slice()
                .try_into()
                .map_err(|_| error(ErrorKind::InvalidRequest, "artifact ID invalid"))?;
            let claims = command_keys
                .verify_artifact_grant(
                    grant,
                    node_id,
                    &artifact_id,
                    "certificate_p12",
                    grant.max_bytes,
                    i64::try_from(now.as_secs())
                        .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?,
                )
                .map_err(|failure| authorization_error(&failure))?;
            validate_artifact_claims(&claims)?;
            verify_artifact_fence(
                value.connection_fence.as_deref(),
                value.fence_binding.as_deref(),
                &claims.artifact_id,
                node_id,
                endpoint_id,
                command_keys,
                i64::try_from(now.as_secs())
                    .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?,
                now.subsec_nanos(),
            )?;
            if value.offset > claims.max_bytes {
                return Err(error(ErrorKind::InvalidRequest, "artifact offset invalid"));
            }
            ValidatedRequest::ArtifactRead(claims, value.offset)
        } else if let Some(privd_request::Operation::ArtifactConsume(value)) =
            request.operation.as_ref()
        {
            let grant = value
                .grant
                .as_ref()
                .ok_or_else(|| error(ErrorKind::PermissionDenied, "artifact grant required"))?;
            let artifact_id: [u8; 16] = grant
                .artifact_id
                .as_slice()
                .try_into()
                .map_err(|_| error(ErrorKind::InvalidRequest, "artifact ID invalid"))?;
            let claims = if value.confirm_only {
                command_keys.verify_artifact_grant_for_confirmation(
                    grant,
                    node_id,
                    &artifact_id,
                    "certificate_p12",
                    grant.max_bytes,
                    i64::try_from(now.as_secs())
                        .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?,
                )
            } else {
                command_keys.verify_artifact_grant(
                    grant,
                    node_id,
                    &artifact_id,
                    "certificate_p12",
                    grant.max_bytes,
                    i64::try_from(now.as_secs())
                        .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?,
                )
            }
            .map_err(|failure| authorization_error(&failure))?;
            validate_artifact_claims(&claims)?;
            verify_artifact_fence(
                value.connection_fence.as_deref(),
                value.fence_binding.as_deref(),
                &claims.grant_id,
                node_id,
                endpoint_id,
                command_keys,
                i64::try_from(now.as_secs())
                    .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?,
                now.subsec_nanos(),
            )?;
            if value.sha256.len() != 32 || value.size == 0 || value.size > claims.max_bytes {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "artifact consumption evidence invalid",
                ));
            }
            ValidatedRequest::ArtifactConsume(
                claims,
                value.sha256.clone(),
                value.size,
                value.confirm_only,
            )
        } else if !matches!(
            request.operation,
            Some(
                privd_request::Operation::ServiceStatus(_)
                    | privd_request::Operation::OcservVersion(_)
                    | privd_request::Operation::SessionList(_)
                    | privd_request::Operation::IpBanList(_)
                    | privd_request::Operation::ConfigFingerprint(_)
                    | privd_request::Operation::UserList(_)
                    | privd_request::Operation::GroupList(_)
                    | privd_request::Operation::UpgradeResultList(_)
            )
        ) {
            return Err(error(
                ErrorKind::PermissionDenied,
                "Controller command authorization required",
            ));
        } else {
            ValidatedRequest::Read
        }
    };
    Ok((remaining, validated))
}

fn validate_artifact_claims(claims: &ArtifactGrantClaimsV1) -> Result<(), PrivdError> {
    if [
        claims.node_id,
        claims.artifact_id,
        claims.certificate_id,
        claims.operation_id,
        claims.grant_id,
    ]
    .into_iter()
    .any(|value| Uuid::from_bytes(value).get_version_num() != 7)
        || claims.certificate_version == 0
        || claims.max_bytes == 0
        || claims.max_bytes > 64 * 1024 * 1024
        || claims.authorized_subject.is_empty()
        || claims.authorized_subject.len() > 256
    {
        return Err(error(
            ErrorKind::InvalidRequest,
            "artifact grant claims invalid",
        ));
    }
    Ok(())
}

fn artifact_identity(claims: &ArtifactGrantClaimsV1) -> ArtifactLeaseIdentity<'_> {
    ArtifactLeaseIdentity {
        artifact_id: &claims.artifact_id,
        certificate_id: &claims.certificate_id,
        certificate_version: claims.certificate_version,
        operation_id: &claims.operation_id,
        authorized_subject: &claims.authorized_subject,
        max_bytes: claims.max_bytes,
        grant_id: &claims.grant_id,
        expires_at_unix_seconds: claims.expires_at_seconds,
    }
}

fn authorization_error(failure: &AuthorizationError) -> PrivdError {
    error(ErrorKind::PermissionDenied, failure.code())
}

/// The Agent's own owner-term checks do not survive an Agent compromise, so
/// root re-verifies the Controller-signed connection fence and its command
/// binding at its own clock before any new effect.
///
/// ponytail: stale authority is bounded by the signed owner lease
/// (`OCSERV_OWNER_LEASE_TTL`, 30s by default); privd keeps no durable
/// owner-epoch/revision floor. Add one to the root effect store if a newer
/// accepted term must revoke an unexpired lease immediately.
fn verify_execution_fence(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<(), PrivdError> {
    let (fence, binding) = verify_fence_carriers(
        command.connection_fence.as_ref(),
        command.fence_binding.as_ref(),
        node_id,
        endpoint_id,
        command_keys,
        now_seconds,
        now_nanos,
    )?;
    if binding.operation_kind != FenceOperationKind::Command as u32
        || binding.operation_id != claims.command_id
        || binding.capability != claims.required_capability
        || claims.expected_revision != fence.authorization_revision
        || !fence.capabilities.contains(&binding.capability)
        || !binding.matches_fence(&fence)
    {
        return Err(error(
            ErrorKind::PermissionDenied,
            "connection fence does not authorize this command",
        ));
    }
    Ok(())
}

/// Verifies the signed owner term and binding carriers against privd's own
/// clock, pinned node ID and root-provisioned `EndpointID`.
fn verify_fence_carriers(
    fence: Option<&ConnectionFenceV2>,
    binding: Option<&FenceBindingV2>,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<(VerifiedConnectionFenceV2, FenceBindingClaimsV2), PrivdError> {
    let (Some(fence), Some(binding)) = (fence, binding) else {
        return Err(error(
            ErrorKind::PermissionDenied,
            "signed connection fence and binding required",
        ));
    };
    let fence = command_keys
        .verify_connection_fence_v2_at(fence, node_id, endpoint_id, now_seconds, now_nanos)
        .map_err(|failure| authorization_error(&failure))?;
    if fence.lease_expired(now_seconds, now_nanos) {
        return Err(error(
            ErrorKind::PermissionDenied,
            "connection-owner lease expired",
        ));
    }
    let binding = command_keys
        .verify_fence_binding_v2_at(binding, node_id, endpoint_id, now_seconds, now_nanos)
        .map_err(|failure| authorization_error(&failure))?;
    Ok((fence, binding))
}

/// Artifact reads and consumption are not commands, so the Agent's owner-term
/// check is re-proven here from the same Controller-signed artifact binding the
/// Agent verified: fetch binds the artifact ID, consumption binds the grant ID.
/// ponytail: like `Execute`, an old binding stays usable until its signed owner
/// lease ends; privd keeps no owner-epoch floor. Each read re-checks the one
/// dispatch fence, which suffices only while a P12 fits one 256 KiB chunk (the
/// 256 KiB chain cap with an RSA-4096 key yields ~192 KiB). If artifacts can
/// span chunks, the Controller must refresh the fence during the transfer.
#[allow(clippy::too_many_arguments)]
fn verify_artifact_fence(
    fence: Option<&ConnectionFenceV2>,
    binding: Option<&FenceBindingV2>,
    operation_id: &[u8; 16],
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<(), PrivdError> {
    let (fence, binding) = verify_fence_carriers(
        fence,
        binding,
        node_id,
        endpoint_id,
        command_keys,
        now_seconds,
        now_nanos,
    )?;
    if binding.operation_kind != FenceOperationKind::Artifact as u32
        || binding.operation_id.as_slice() != operation_id
        || binding.capability != CONNECTION_FENCING_CAPABILITY
        || !fence.capabilities.contains(&binding.capability)
        || !binding.matches_fence(&fence)
    {
        return Err(error(
            ErrorKind::PermissionDenied,
            "connection fence does not authorize this artifact operation",
        ));
    }
    Ok(())
}

#[allow(clippy::too_many_lines)]
fn validate_privileged_payload(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
) -> Result<(), PrivdError> {
    for value in [
        claims.command_id,
        claims.idempotency_key,
        claims.node_id,
        claims.operation_id,
    ] {
        if Uuid::from_bytes(value).get_version_num() != 7 {
            return Err(error(
                ErrorKind::InvalidRequest,
                "signed command UUID identity invalid",
            ));
        }
    }
    match command.payload.as_ref() {
        Some(
            command_envelope::Payload::CompleteConfigPlan(_)
            | command_envelope::Payload::CompleteConfigApply(_),
        ) => {
            ocservia_command_authorization::semantic_payload_hash_v2(command)
                .map_err(|failure| authorization_error(&failure))?;
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            validate_session(&payload.session_id, &payload.boot_id)?;
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            validate_session(&payload.session_id, &payload.boot_id)?;
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            let canonical = payload
                .ip
                .parse::<std::net::IpAddr>()
                .map_err(|_| error(ErrorKind::InvalidRequest, "IP address invalid"))?
                .to_string();
            if canonical != payload.ip {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "IP address must be canonical",
                ));
            }
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            if payload.members.len() > ocservia_agent_protocol::MAX_MANAGED_RESOURCES
                || payload.members.windows(2).any(|pair| pair[0] >= pair[1])
            {
                return Err(error(ErrorKind::InvalidRequest, "group members invalid"));
            }
        }
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            validate_uuid(&payload.certificate_id, "certificate ID invalid")?;
        }
        Some(command_envelope::Payload::CertificateRevoke(payload)) => {
            validate_uuid(&payload.certificate_id, "certificate ID invalid")?;
            if payload.certificate_version == 0 {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "certificate version invalid",
                ));
            }
        }
        Some(command_envelope::Payload::CertificateP12(payload)) => {
            validate_uuid(&payload.certificate_id, "certificate ID invalid")?;
            validate_uuid(&payload.artifact_id, "artifact ID invalid")?;
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::CertificateP12Password,
            )?;
            if !payload.sealed_password.is_empty()
                || !payload.secret_key_id.is_empty()
                || payload.certificate_version == 0
                || payload.artifact_expires_at.is_none()
            {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "P12 secret binding invalid",
                ));
            }
        }
        Some(command_envelope::Payload::UserCreate(payload)) => {
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::UserPassword,
            )?;
            if !payload.sealed_password.is_empty() || !payload.secret_key_id.is_empty() {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "user secret binding invalid",
                ));
            }
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::UserPassword,
            )?;
            if !payload.sealed_password.is_empty() || !payload.secret_key_id.is_empty() {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "user secret binding invalid",
                ));
            }
        }
        Some(
            command_envelope::Payload::ServiceReload(_)
            | command_envelope::Payload::UserDisable(_)
            | command_envelope::Payload::UserEnable(_),
        ) => {}
        Some(command_envelope::Payload::AgentUpgrade(payload)) => {
            validate_upgrade_release(payload)?;
        }
        _ => {
            return Err(error(
                ErrorKind::PermissionDenied,
                "payload is not a privileged privd command",
            ));
        }
    }
    Ok(())
}

fn validate_sealed_secret(
    secret: Option<&ocservia_contracts::generated::ocserv::platform::agent::v1::SealedSecretV1>,
    purpose: SealedSecretPurpose,
) -> Result<(), PrivdError> {
    let secret =
        secret.ok_or_else(|| error(ErrorKind::InvalidRequest, "sealed secret required"))?;
    if SealedSecretVersion::try_from(secret.version).ok() != Some(SealedSecretVersion::V1)
        || SealedSecretPurpose::try_from(secret.purpose).ok() != Some(purpose)
        || secret.key_id.is_empty()
        || secret.key_id.len() > 128
        || secret.ciphertext.len() < 32
        || secret.ciphertext.len() > 16 * 1024
    {
        return Err(error(ErrorKind::InvalidRequest, "sealed secret invalid"));
    }
    Ok(())
}

/// Independently validates the immutable upgrade release identity carried by a
/// Controller-signed command. Privd never trusts the Agent-side validation and
/// never executes package installation at this boundary.
fn validate_upgrade_release(
    payload: &ocservia_contracts::generated::ocserv::platform::agent::v1::AgentUpgrade,
) -> Result<(), PrivdError> {
    if !ocservia_contracts::agent_upgrade::valid_target_version(&payload.target_version) {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade target version invalid",
        ));
    }
    if payload.package_sha256.len() != 32 {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade package digest invalid",
        ));
    }
    if !ocservia_contracts::agent_upgrade::valid_architecture(&payload.architecture) {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade architecture invalid",
        ));
    }
    if ocservia_contracts::agent_upgrade::runtime_architecture()
        != Some(payload.architecture.as_str())
    {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade architecture does not match this host",
        ));
    }
    Ok(())
}

fn validate_session(session_id: &str, boot_id: &str) -> Result<(), PrivdError> {
    let parsed = session_id
        .parse::<u64>()
        .map_err(|_| error(ErrorKind::InvalidRequest, "session ID invalid"))?;
    if parsed == 0 || parsed.to_string() != session_id || Uuid::parse_str(boot_id).is_err() {
        return Err(error(ErrorKind::InvalidRequest, "session target invalid"));
    }
    Ok(())
}

fn validate_uuid(value: &[u8], detail: &str) -> Result<(), PrivdError> {
    let id = Uuid::from_slice(value).map_err(|_| error(ErrorKind::InvalidRequest, detail))?;
    if id.get_version_num() != 7 {
        return Err(error(ErrorKind::InvalidRequest, detail));
    }
    Ok(())
}

#[allow(clippy::too_many_lines)]
async fn execute_read(
    operation: Option<privd_request::Operation>,
    node_id: &[u8; 16],
    attestation_key: &SigningKey,
    adapter: &Adapter,
    upgrades: &UpgradeScheduler,
) -> privd_response::Result {
    let Some(operation) = operation else {
        return privd_response::Result::Error(error(
            ErrorKind::InvalidRequest,
            "read-only operation required",
        ));
    };
    let (operation_name, result) = match operation {
        privd_request::Operation::ServiceStatus(_) => (
            "service_status",
            adapter
                .service_status()
                .await
                .map(privd_response::Result::ServiceStatus),
        ),
        privd_request::Operation::OcservVersion(_) => (
            "ocserv_version",
            adapter
                .ocserv_version()
                .await
                .map(privd_response::Result::OcservVersion),
        ),
        privd_request::Operation::SessionList(_) => (
            "session_list",
            adapter
                .session_list()
                .await
                .map(privd_response::Result::SessionList),
        ),
        privd_request::Operation::IpBanList(_) => (
            "ip_ban_list",
            adapter
                .ip_ban_list()
                .await
                .map(privd_response::Result::IpBanList),
        ),
        privd_request::Operation::ConfigFingerprint(_) => (
            "config_fingerprint",
            adapter
                .config_fingerprint()
                .await
                .map(privd_response::Result::ConfigFingerprint),
        ),
        privd_request::Operation::UserList(_) => (
            "user_list",
            adapter
                .user_list()
                .await
                .map(privd_response::Result::UserList),
        ),
        privd_request::Operation::GroupList(_) => (
            "group_list",
            adapter
                .group_list()
                .await
                .map(privd_response::Result::GroupList),
        ),
        privd_request::Operation::UpgradeResultList(_) => (
            "upgrade_result_list",
            ocservia_upgrader::read_recent_results(upgrades.operations_root(), 8)
                .and_then(|results| {
                    results
                        .into_iter()
                        .map(|result| {
                            let completed_unix_ms = result.completed_unix.checked_mul(1000).ok_or(
                                UpgradeStoreError::Unsafe(
                                    "durable result completion time overflows milliseconds",
                                ),
                            )?;
                            let state = match result.state {
                                ocservia_upgrader::OperationState::Succeeded => {
                                    AgentUpgradeOutcomeState::Succeeded
                                }
                                ocservia_upgrader::OperationState::Failed => {
                                    AgentUpgradeOutcomeState::Failed
                                }
                                ocservia_upgrader::OperationState::RolledBack => {
                                    AgentUpgradeOutcomeState::RolledBack
                                }
                                ocservia_upgrader::OperationState::Accepted
                                | ocservia_upgrader::OperationState::Running => {
                                    return Err(UpgradeStoreError::Unsafe(
                                        "durable result is not terminal",
                                    ));
                                }
                            };
                            // The signature time, not the historical completion
                            // time, carries key validity: a key rotated after
                            // completion may still attest the older result.
                            let attested_unix_ms = u64::try_from(
                                SystemTime::now()
                                    .duration_since(UNIX_EPOCH)
                                    .map_err(|_| {
                                        UpgradeStoreError::Unsafe(
                                            "system clock precedes the unix epoch",
                                        )
                                    })?
                                    .as_millis(),
                            )
                            .map_err(|_| {
                                UpgradeStoreError::Unsafe("attestation time overflows milliseconds")
                            })?;
                            let proof = sign_upgrade_result(
                                AgentUpgradeResultProof {
                                    version: PrivdReceiptVersion::V1.into(),
                                    node_id: node_id.to_vec(),
                                    privd_attestation_key_id: key_id(
                                        &attestation_key.verifying_key(),
                                    ),
                                    operation_id: result.operation_id.as_bytes().to_vec(),
                                    target_version: result.target_version.clone(),
                                    package_sha256: result.package_sha256.to_vec(),
                                    state: state.into(),
                                    completed_unix_ms,
                                    result_sha256: result.result_sha256.to_vec(),
                                    attested_unix_ms,
                                    signature: Vec::new(),
                                },
                                attestation_key,
                            )
                            .map_err(|_| {
                                UpgradeStoreError::Unsafe(
                                    "durable result proof claims are malformed",
                                )
                            })?;
                            Ok(UpgradeOperationResult {
                                operation_id: result.operation_id.as_bytes().to_vec(),
                                state: result.state.as_str().to_owned(),
                                target_version: result.target_version,
                                completed_unix_ms,
                                detail: result.detail,
                                package_sha256: result.package_sha256.to_vec(),
                                privileged_result_proof: Some(proof),
                            })
                        })
                        .collect::<Result<Vec<_>, UpgradeStoreError>>()
                        .map(|results| {
                            privd_response::Result::UpgradeResultList(UpgradeResultList { results })
                        })
                })
                .map_err(|failure| {
                    ocservia_ocserv_adapter::AdapterError::Io(io::Error::other(failure.to_string()))
                }),
        ),
        _ => {
            return privd_response::Result::Error(error(
                ErrorKind::PermissionDenied,
                "Controller command authorization required",
            ));
        }
    };
    finish_operation(operation_name, result)
}

struct CommandEffectBinding {
    effect_kind: &'static str,
    resource_key: String,
    effect_revision: u64,
}

#[allow(clippy::too_many_lines)]
fn effect_binding(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
) -> Option<CommandEffectBinding> {
    let (effect_kind, resource_key, effect_revision) = match command.payload.as_ref()? {
        command_envelope::Payload::SessionDisconnect(payload) => (
            "session_disconnect",
            format!("{}:{}", payload.boot_id, payload.session_id),
            claims.expected_revision,
        ),
        command_envelope::Payload::SessionTerminate(payload) => (
            "session_terminate",
            format!("{}:{}", payload.boot_id, payload.session_id),
            claims.expected_revision,
        ),
        command_envelope::Payload::IpBanRemove(payload) => (
            "ip_ban_remove",
            payload.ip.clone(),
            claims.expected_revision,
        ),
        command_envelope::Payload::ServiceReload(_) => (
            "service_reload",
            "ocserv.service".to_owned(),
            claims.expected_revision,
        ),
        command_envelope::Payload::UserCreate(payload) => (
            "user_create",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserDisable(payload) => (
            "user_disable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserEnable(payload) => (
            "user_enable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserPasswordRotate(payload) => (
            "user_password_rotate",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::GroupApply(payload) => (
            "group_apply",
            payload.group_name.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::CompleteConfigPlan(payload) => (
            "config_plan",
            hex::encode(&payload.candidate_hash),
            claims.expected_revision,
        ),
        command_envelope::Payload::CompleteConfigApply(payload) => (
            "config_apply",
            "ocserv.conf".to_owned(),
            payload.desired_revision,
        ),
        command_envelope::Payload::CertificateCsr(payload) => (
            "certificate_csr",
            Uuid::from_slice(&payload.certificate_id).ok()?.to_string(),
            claims.expected_revision,
        ),
        command_envelope::Payload::CertificateRevoke(payload) => (
            "certificate_revoke",
            Uuid::from_slice(&payload.certificate_id).ok()?.to_string(),
            claims.expected_revision,
        ),
        command_envelope::Payload::CertificateP12(payload) => (
            "certificate_p12",
            format!(
                "{}:{}",
                Uuid::from_slice(&payload.certificate_id).ok()?,
                Uuid::from_slice(&payload.artifact_id).ok()?
            ),
            claims.expected_revision,
        ),
        command_envelope::Payload::AgentUpgrade(payload) => (
            "agent_upgrade_prepare",
            format!(
                "{}:{}",
                payload.target_version,
                hex::encode(&payload.package_sha256)
            ),
            claims.expected_revision,
        ),
        _ => return None,
    };
    Some(CommandEffectBinding {
        effect_kind,
        resource_key,
        effect_revision,
    })
}

fn desired_effect_binding(command: &CommandEnvelope) -> Option<CommandEffectBinding> {
    let (effect_kind, resource_key, effect_revision) = match command.payload.as_ref()? {
        command_envelope::Payload::UserCreate(payload) => (
            "user_create",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserDisable(payload) => (
            "user_disable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserEnable(payload) => (
            "user_enable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserPasswordRotate(payload) => (
            "user_password_rotate",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::GroupApply(payload) => (
            "group_apply",
            payload.group_name.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::CompleteConfigApply(payload) => (
            "config_apply",
            "ocserv.conf".to_owned(),
            payload.desired_revision,
        ),
        _ => return None,
    };
    Some(CommandEffectBinding {
        effect_kind,
        resource_key,
        effect_revision,
    })
}

fn adapter_effect(claims: &CommandAuthorizationV1) -> EffectIdentity<'_> {
    EffectIdentity {
        command_id: &claims.command_id,
        idempotency_key: &claims.idempotency_key,
        semantic_payload_sha256: &claims.semantic_payload_sha256,
        expires_at_unix_seconds: claims.expires_at_seconds,
    }
}

fn authorized_effect<'a>(
    command: &CommandEnvelope,
    claims: &'a CommandAuthorizationV1,
    binding: &'a CommandEffectBinding,
) -> AuthorizedEffectIdentity<'a> {
    AuthorizedEffectIdentity {
        node_id: &claims.node_id,
        command_id: &claims.command_id,
        operation_id: &claims.operation_id,
        idempotency_key: &claims.idempotency_key,
        semantic_payload_sha256: &claims.semantic_payload_sha256,
        action: &claims.action,
        authorization_revision: claims.expected_revision,
        effect_kind: binding.effect_kind,
        resource_key: &binding.resource_key,
        effect_revision: binding.effect_revision,
        expires_at_unix_seconds: claims.expires_at_seconds,
        retry_if_absent: CommandDeliveryMode::try_from(command.delivery_mode)
            .is_ok_and(|mode| mode == CommandDeliveryMode::RetryIfEffectAbsent),
    }
}

/// Session, ban-removal and reload effects leave no root-verifiable proof that
/// they did not happen, and the Agent's absence report is unsigned. A retry
/// grant for them (including one signed before this rule) therefore never
/// reaches the executor; it can only return an already attested result.
fn fixed_host_effect(command: &CommandEnvelope) -> bool {
    matches!(
        command.payload.as_ref(),
        Some(
            command_envelope::Payload::SessionDisconnect(_)
                | command_envelope::Payload::SessionTerminate(_)
                | command_envelope::Payload::IpBanRemove(_)
                | command_envelope::Payload::ServiceReload(_)
        )
    )
}

fn replay_only_response(
    adapter: &Adapter,
    identity: AuthorizedEffectIdentity<'_>,
) -> PrivdResponse {
    match adapter.replay_authorized_effect(identity) {
        Ok(Some(encoded)) => decode_cached_response(&encoded),
        // Missing, pruned or still-prepared evidence cannot prove absence.
        Ok(None) => response_error(
            ErrorKind::Unavailable,
            "authorized effect outcome requires reconciliation",
        ),
        Err(failure) => PrivdResponse {
            request_id: Vec::new(),
            privileged_result_proof: None,
            result: Some(privd_response::Result::Error(PrivdError::from(failure))),
        },
    }
}

async fn execute_command(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    accepted_at: &prost_types::Timestamp,
    node_id: &[u8; 16],
    attestation_key: &SigningKey,
    upgrades: &UpgradeScheduler,
    adapter: &Adapter,
) -> PrivdResponse {
    let binding = effect_binding(command, claims);
    if let Some(binding) = binding.as_ref() {
        let identity = authorized_effect(command, claims, binding);
        // Legacy recovery grants may have renewed an already expired deadline.
        if identity.retry_if_absent
            && (fixed_host_effect(command)
                || command.protocol_version
                    == ocservia_command_authorization::LEGACY_COMMAND_PROTOCOL_VERSION)
        {
            return replay_only_response(adapter, identity);
        }
        match adapter.prepare_authorized_effect(identity) {
            Ok(AuthorizedEffectDecision::Execute) => {}
            Ok(AuthorizedEffectDecision::Replay(encoded)) => {
                return decode_cached_response(&encoded);
            }
            // For an agent upgrade a Pending effect means the journal lost
            // the completed response (for example privd restarted between
            // the durable intent commit and the journal completion). The
            // immutable intent store is the idempotency authority for this
            // family, so the retry continues into scheduling instead of
            // stalling on an Unknown outcome.
            Ok(AuthorizedEffectDecision::Pending)
                if matches!(
                    command.payload.as_ref(),
                    Some(command_envelope::Payload::AgentUpgrade(_))
                ) => {}
            Ok(AuthorizedEffectDecision::Pending) => {
                return response_error(
                    ErrorKind::Unavailable,
                    "authorized effect outcome requires reconciliation",
                );
            }
            Err(failure) => {
                return PrivdResponse {
                    request_id: Vec::new(),
                    privileged_result_proof: None,
                    result: Some(privd_response::Result::Error(PrivdError::from(failure))),
                };
            }
        }
    }
    let (operation_name, result) =
        if let Some(command_envelope::Payload::AgentUpgrade(payload)) = command.payload.as_ref() {
            (
                "agent_upgrade_prepare",
                schedule_agent_upgrade(payload, claims, upgrades),
            )
        } else {
            execute_signed_payload(command, claims, adapter).await
        };
    let result = finish_operation(operation_name, result);
    if matches!(&result, privd_response::Result::Error(error) if terminal_result_error_code(error).is_none())
    {
        return PrivdResponse {
            request_id: Vec::new(),
            privileged_result_proof: None,
            result: Some(result),
        };
    }
    let Some(binding) = binding.as_ref() else {
        return response_error(ErrorKind::Unavailable, "root effect identity unavailable");
    };
    let Ok(proof) = build_result_proof(
        command,
        claims,
        accepted_at,
        node_id,
        binding,
        &result,
        attestation_key,
    ) else {
        return response_error(ErrorKind::Unavailable, "result attestation unavailable");
    };
    let response = PrivdResponse {
        request_id: Vec::new(),
        privileged_result_proof: Some(proof),
        result: Some(result),
    };
    let cached = response.encode_to_vec();
    if let Err(failure) =
        adapter.complete_authorized_effect(authorized_effect(command, claims, binding), &cached)
    {
        return PrivdResponse {
            request_id: Vec::new(),
            privileged_result_proof: None,
            result: Some(privd_response::Result::Error(PrivdError::from(failure))),
        };
    }
    response
}

/// The durable self-upgrade preparation boundary. privd independently
/// verified the Controller-signed release identity, commits the immutable
/// root-owned intent, starts the fixed upgrader unit, and returns only the
/// scheduled acknowledgment. It never executes the upgrade itself, so a
/// normal upgrade cannot destroy the process that owes the command result.
fn schedule_agent_upgrade(
    payload: &AgentUpgrade,
    claims: &CommandAuthorizationV1,
    upgrades: &UpgradeScheduler,
) -> Result<privd_response::Result, ocservia_ocserv_adapter::AdapterError> {
    let package_sha256: [u8; 32] = payload
        .package_sha256
        .clone()
        .try_into()
        .map_err(|_| ocservia_ocserv_adapter::AdapterError::InvalidRequest)?;
    let intent = UpgradeIntent::new(
        claims.operation_id,
        claims.command_id,
        &payload.target_version,
        package_sha256,
        &payload.architecture,
        claims.semantic_payload_sha256,
    )
    .map_err(|failure| upgrade_store_error(&failure))?;
    match upgrades.schedule_and_trigger(&intent) {
        Ok(ScheduleOutcome::Scheduled | ScheduleOutcome::AlreadyScheduled) => Ok(
            privd_response::Result::AgentUpgradeScheduled(AgentUpgradeScheduledResult {
                operation_id: claims.operation_id.to_vec(),
                target_version: payload.target_version.clone(),
                package_sha256: payload.package_sha256.clone(),
            }),
        ),
        Err(failure) => Err(upgrade_store_error(&failure)),
    }
}

fn upgrade_store_error(failure: &UpgradeStoreError) -> ocservia_ocserv_adapter::AdapterError {
    tracing::warn!(error = %failure, "durable upgrade scheduling refused");
    match failure {
        UpgradeStoreError::Io(_) | UpgradeStoreError::Lifecycle(_) => {
            ocservia_ocserv_adapter::AdapterError::Unavailable
        }
        UpgradeStoreError::Invalid(_)
        | UpgradeStoreError::IdentityConflict
        | UpgradeStoreError::ActiveConflict
        | UpgradeStoreError::Unsafe(_)
        | UpgradeStoreError::Package(_) => ocservia_ocserv_adapter::AdapterError::InvalidRequest,
    }
}

#[allow(clippy::too_many_lines)]
async fn execute_signed_payload(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    adapter: &Adapter,
) -> (
    &'static str,
    Result<privd_response::Result, ocservia_ocserv_adapter::AdapterError>,
) {
    let effect = adapter_effect(claims);
    match command.payload.as_ref() {
        Some(command_envelope::Payload::SessionDisconnect(payload)) => (
            "session_disconnect",
            adapter
                .session_disconnect(&payload.session_id, &payload.boot_id)
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::SessionTerminate(payload)) => (
            "session_terminate",
            adapter
                .session_terminate(&payload.session_id, &payload.boot_id)
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::IpBanRemove(payload)) => (
            "ip_ban_remove",
            adapter
                .ip_ban_remove(&payload.ip)
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::ServiceReload(_)) => (
            "service_reload",
            adapter
                .service_reload()
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::UserCreate(payload)) => (
            "user_create",
            adapter
                .user_create(
                    &payload.username,
                    &payload
                        .sealed_password_v1
                        .as_ref()
                        .expect("validated secret")
                        .key_id,
                    &payload
                        .sealed_password_v1
                        .as_ref()
                        .expect("validated secret")
                        .ciphertext,
                    payload.desired_revision,
                    effect,
                )
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::UserDisable(payload)) => (
            "user_disable",
            adapter
                .user_disable(&payload.username, payload.desired_revision, effect)
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::UserEnable(payload)) => (
            "user_enable",
            adapter
                .user_enable(&payload.username, payload.desired_revision, effect)
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => (
            "user_password_rotate",
            adapter
                .user_password_rotate(
                    &payload.username,
                    &payload
                        .sealed_password_v1
                        .as_ref()
                        .expect("validated secret")
                        .key_id,
                    &payload
                        .sealed_password_v1
                        .as_ref()
                        .expect("validated secret")
                        .ciphertext,
                    payload.desired_revision,
                    effect,
                )
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::GroupApply(payload)) => (
            "group_apply",
            adapter
                .group_apply(
                    &payload.group_name,
                    &payload.members,
                    payload.desired_revision,
                    effect,
                )
                .await
                .map(privd_response::Result::Mutation),
        ),
        Some(command_envelope::Payload::CompleteConfigPlan(payload)) => (
            "config_plan",
            adapter
                .complete_config_plan(payload)
                .await
                .map(privd_response::Result::ConfigPlan),
        ),
        Some(command_envelope::Payload::CompleteConfigApply(payload)) => (
            "config_apply",
            adapter
                .complete_config_apply(payload, effect)
                .await
                .map(privd_response::Result::ConfigApply),
        ),
        Some(command_envelope::Payload::CertificateCsr(payload)) => (
            "certificate_csr",
            adapter
                .certificate_csr(
                    &payload.certificate_id,
                    &payload.common_name,
                    &payload.dns_names,
                    payload.key_bits,
                )
                .await
                .map(privd_response::Result::CertificateCsr),
        ),
        Some(command_envelope::Payload::CertificateRevoke(payload)) => (
            "certificate_revoke",
            adapter
                .certificate_revoke(&payload.certificate_id, payload.certificate_version)
                .await
                .map(privd_response::Result::CertificateRevoke),
        ),
        Some(command_envelope::Payload::CertificateP12(payload)) => (
            "certificate_p12",
            adapter
                .certificate_p12(
                    &payload.certificate_id,
                    &payload.artifact_id,
                    &payload.certificate_chain_pem,
                    payload
                        .sealed_password_v1
                        .as_ref()
                        .expect("validated secret"),
                    payload.certificate_version,
                    &claims.operation_id,
                    payload
                        .artifact_expires_at
                        .as_ref()
                        .expect("validated expiry")
                        .seconds,
                )
                .await
                .map(privd_response::Result::CertificateP12),
        ),
        // AgentUpgrade payloads never reach this executor: privd intercepts
        // them in `execute_command` and hands them to the durable scheduler,
        // because the upgrade must survive the Agent/privd restart it causes.
        _ => (
            "privileged_command",
            Err(ocservia_ocserv_adapter::AdapterError::InvalidRequest),
        ),
    }
}

async fn reconcile_command(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    adapter: &Adapter,
) -> PrivdResponse {
    let Some(root_binding) = effect_binding(command, claims) else {
        return response_error(ErrorKind::InvalidRequest, "root effect binding unavailable");
    };
    match adapter.replay_authorized_effect(authorized_effect(command, claims, &root_binding)) {
        Ok(Some(encoded)) => return decode_cached_response(&encoded),
        Ok(None) => {}
        Err(error) => {
            return PrivdResponse {
                request_id: Vec::new(),
                privileged_result_proof: None,
                result: Some(privd_response::Result::Error(PrivdError::from(error))),
            };
        }
    }
    let Some(binding) = desired_effect_binding(command) else {
        return response_error(
            ErrorKind::InvalidRequest,
            "desired effect binding unavailable",
        );
    };
    let result = adapter
        .desired_effect_observe(
            binding.effect_kind,
            &binding.resource_key,
            binding.effect_revision,
            adapter_effect(claims),
        )
        .await
        .map(privd_response::Result::DesiredEffectObservation);
    PrivdResponse {
        request_id: Vec::new(),
        privileged_result_proof: None,
        result: Some(finish_operation("desired_effect_observe", result)),
    }
}

fn decode_cached_response(encoded: &[u8]) -> PrivdResponse {
    match PrivdResponse::decode(encoded) {
        Ok(response)
            if response.request_id.is_empty()
                && response.result.is_some()
                && response.privileged_result_proof.is_some() =>
        {
            response
        }
        _ => response_error(
            ErrorKind::Unavailable,
            "cached authorized effect result invalid",
        ),
    }
}

fn response_error(kind: ErrorKind, detail: &'static str) -> PrivdResponse {
    PrivdResponse {
        request_id: Vec::new(),
        privileged_result_proof: None,
        result: Some(privd_response::Result::Error(error(kind, detail))),
    }
}

fn build_result_proof(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    accepted_at: &prost_types::Timestamp,
    node_id: &[u8; 16],
    binding: &CommandEffectBinding,
    result: &privd_response::Result,
    key: &SigningKey,
) -> Result<ocservia_contracts::generated::ocserv::platform::agent::v1::PrivilegedResultProof, ()> {
    let completed = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| ())?;
    let completed_at = prost_types::Timestamp {
        seconds: i64::try_from(completed.as_secs()).map_err(|_| ())?,
        nanos: i32::try_from(completed.subsec_nanos()).map_err(|_| ())?,
    };
    let result_bytes = exact_result_bytes(result).ok_or(())?;
    let mut effect = Sha256::new();
    effect.update(b"ocservia/privd-root-effect-record/v1\0");
    effect.update(node_id);
    effect.update(claims.command_id);
    effect.update(claims.operation_id);
    effect.update(claims.idempotency_key);
    effect.update(binding.effect_kind.as_bytes());
    effect.update((binding.resource_key.len() as u64).to_be_bytes());
    effect.update(binding.resource_key.as_bytes());
    effect.update(binding.effect_revision.to_be_bytes());
    let effect_record_id = effect.finalize().to_vec();
    let certificate = certificate_binding(command, result, &effect_record_id)?;
    let receipt = PrivdResultReceiptV1 {
        receipt_version: PrivdReceiptVersion::V1.into(),
        node_id: node_id.to_vec(),
        privd_attestation_key_id: key_id(&key.verifying_key()),
        command_id: claims.command_id.to_vec(),
        operation_id: claims.operation_id.to_vec(),
        idempotency_key: claims.idempotency_key.to_vec(),
        semantic_payload_hash_version: i32::try_from(claims.semantic_hash_version)
            .map_err(|_| ())?,
        semantic_payload_sha256: claims.semantic_payload_sha256.to_vec(),
        command_kind: privileged_command_kind(command).ok_or(())?.into(),
        result_kind: privileged_result_kind(result).ok_or(())?.into(),
        terminal_state: match result {
            privd_response::Result::Error(_) => CommandResultState::Failed.into(),
            _ => CommandResultState::Succeeded.into(),
        },
        result_bytes_sha256: Sha256::digest(&result_bytes).to_vec(),
        error_code_sha256: Sha256::digest(match result {
            privd_response::Result::Error(error) => {
                terminal_result_error_code(error).ok_or(())?.as_bytes()
            }
            _ => &[],
        })
        .to_vec(),
        effect_record_id,
        effect_sequence: binding.effect_revision.max(claims.expected_revision).max(1),
        accepted_at: Some(*accepted_at),
        completed_at: Some(completed_at),
        replayed: false,
        certificate,
    };
    sign_receipt(receipt, key).map_err(|_| ())
}

fn exact_result_bytes(result: &privd_response::Result) -> Option<Vec<u8>> {
    match result {
        privd_response::Result::Mutation(value) => Some(value.encode_to_vec()),
        privd_response::Result::ConfigPlan(value) => Some(value.encode_to_vec()),
        privd_response::Result::ConfigApply(value) => Some(value.encode_to_vec()),
        privd_response::Result::CertificateCsr(value) => Some(value.encode_to_vec()),
        privd_response::Result::CertificateP12(value) => Some(value.encode_to_vec()),
        privd_response::Result::CertificateRevoke(value) => Some(value.encode_to_vec()),
        privd_response::Result::AgentUpgradeScheduled(value) => Some(value.encode_to_vec()),
        privd_response::Result::Error(error) if terminal_result_error_code(error).is_some() => {
            Some(Vec::new())
        }
        _ => None,
    }
}

fn privileged_result_kind(result: &privd_response::Result) -> Option<PrivilegedResultKind> {
    match result {
        privd_response::Result::Mutation(_) => Some(PrivilegedResultKind::Mutation),
        privd_response::Result::ConfigPlan(_) => Some(PrivilegedResultKind::ConfigPlan),
        privd_response::Result::ConfigApply(_) => Some(PrivilegedResultKind::ConfigApply),
        privd_response::Result::CertificateCsr(_) => Some(PrivilegedResultKind::CertificateCsr),
        privd_response::Result::CertificateP12(_) => Some(PrivilegedResultKind::CertificateP12),
        privd_response::Result::CertificateRevoke(_) => {
            Some(PrivilegedResultKind::CertificateRevoke)
        }
        privd_response::Result::AgentUpgradeScheduled(_) => {
            Some(PrivilegedResultKind::AgentUpgradeScheduled)
        }
        privd_response::Result::Error(error) if terminal_result_error_code(error).is_some() => {
            Some(PrivilegedResultKind::Error)
        }
        _ => None,
    }
}

fn privileged_command_kind(command: &CommandEnvelope) -> Option<PrivilegedCommandKind> {
    match command.payload.as_ref()? {
        command_envelope::Payload::SessionDisconnect(_) => {
            Some(PrivilegedCommandKind::SessionDisconnect)
        }
        command_envelope::Payload::SessionTerminate(_) => {
            Some(PrivilegedCommandKind::SessionTerminate)
        }
        command_envelope::Payload::IpBanRemove(_) => Some(PrivilegedCommandKind::IpBanRemove),
        command_envelope::Payload::ServiceReload(_) => Some(PrivilegedCommandKind::ServiceReload),
        command_envelope::Payload::UserCreate(_) => Some(PrivilegedCommandKind::UserCreate),
        command_envelope::Payload::UserDisable(_) => Some(PrivilegedCommandKind::UserDisable),
        command_envelope::Payload::UserEnable(_) => Some(PrivilegedCommandKind::UserEnable),
        command_envelope::Payload::UserPasswordRotate(_) => {
            Some(PrivilegedCommandKind::UserPasswordRotate)
        }
        command_envelope::Payload::GroupApply(_) => Some(PrivilegedCommandKind::GroupApply),
        command_envelope::Payload::CompleteConfigPlan(_) => Some(PrivilegedCommandKind::ConfigPlan),
        command_envelope::Payload::CompleteConfigApply(_) => {
            Some(PrivilegedCommandKind::ConfigApply)
        }
        command_envelope::Payload::CertificateCsr(_) => Some(PrivilegedCommandKind::CertificateCsr),
        command_envelope::Payload::CertificateP12(_) => Some(PrivilegedCommandKind::CertificateP12),
        command_envelope::Payload::CertificateRevoke(_) => {
            Some(PrivilegedCommandKind::CertificateRevoke)
        }
        command_envelope::Payload::AgentUpgrade(_) => Some(PrivilegedCommandKind::AgentUpgrade),
        _ => None,
    }
}

fn certificate_binding(
    command: &CommandEnvelope,
    result: &privd_response::Result,
    effect_record_id: &[u8],
) -> Result<Option<PrivdCertificateReceiptBindingV1>, ()> {
    match (command.payload.as_ref(), result) {
        (
            Some(command_envelope::Payload::CertificateCsr(request)),
            privd_response::Result::CertificateCsr(response),
        ) => Ok(Some(PrivdCertificateReceiptBindingV1 {
            certificate_id: request.certificate_id.clone(),
            csr_der_sha256: Sha256::digest(&response.csr_der).to_vec(),
            public_key_sha256: response.public_key_sha256.clone(),
            requested_subject_sha256: requested_subject_digest(request).map_err(|_| ())?.to_vec(),
            root_effect_record_id: effect_record_id.to_vec(),
        })),
        (
            Some(command_envelope::Payload::CertificateCsr(request)),
            privd_response::Result::Error(error),
        ) if terminal_result_error_code(error).is_some() => {
            Ok(Some(PrivdCertificateReceiptBindingV1 {
                certificate_id: request.certificate_id.clone(),
                csr_der_sha256: Vec::new(),
                public_key_sha256: Vec::new(),
                requested_subject_sha256: Vec::new(),
                root_effect_record_id: effect_record_id.to_vec(),
            }))
        }
        (Some(command_envelope::Payload::CertificateP12(request)), _) => {
            Ok(Some(PrivdCertificateReceiptBindingV1 {
                certificate_id: request.certificate_id.clone(),
                csr_der_sha256: Vec::new(),
                public_key_sha256: Vec::new(),
                requested_subject_sha256: Vec::new(),
                root_effect_record_id: effect_record_id.to_vec(),
            }))
        }
        (Some(command_envelope::Payload::CertificateRevoke(request)), _) => {
            Ok(Some(PrivdCertificateReceiptBindingV1 {
                certificate_id: request.certificate_id.clone(),
                csr_der_sha256: Vec::new(),
                public_key_sha256: Vec::new(),
                requested_subject_sha256: Vec::new(),
                root_effect_record_id: effect_record_id.to_vec(),
            }))
        }
        (Some(command_envelope::Payload::CertificateCsr(_)), _) => Err(()),
        _ => Ok(None),
    }
}

fn terminal_result_error_code(error: &PrivdError) -> Option<&'static str> {
    match ErrorKind::try_from(error.kind).unwrap_or(ErrorKind::Unspecified) {
        ErrorKind::CapacityExceeded => Some("capacity_exceeded"),
        ErrorKind::InvalidRequest | ErrorKind::PermissionDenied | ErrorKind::MalformedOutput => {
            Some("privd_rejected")
        }
        _ => None,
    }
}

fn finish_operation(
    operation_name: &str,
    result: Result<privd_response::Result, ocservia_ocserv_adapter::AdapterError>,
) -> privd_response::Result {
    let exit_code = match &result {
        Err(ocservia_ocserv_adapter::AdapterError::CommandFailed { code }) => *code,
        _ => None,
    };
    let result =
        result.unwrap_or_else(|failure| privd_response::Result::Error(PrivdError::from(failure)));
    if let privd_response::Result::Error(mut error) = result {
        tracing::warn!(
            operation = operation_name,
            error_kind = error.kind,
            exit_code,
            "privd operation failed"
        );
        error.detail = format!("{operation_name}: {}", error.detail);
        privd_response::Result::Error(error)
    } else {
        result
    }
}

fn deadline_error() -> privd_response::Result {
    privd_response::Result::Error(error(
        ErrorKind::DeadlineExceeded,
        "request deadline exceeded",
    ))
}

fn error(kind: ErrorKind, detail: &str) -> PrivdError {
    PrivdError {
        kind: kind.into(),
        detail: detail.to_owned(),
    }
}

/// Removes only the socket inode created by this server.
///
/// # Errors
///
/// Refuses non-socket replacement.
pub fn remove_socket(path: &Path) -> Result<(), io::Error> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_socket() && metadata.uid() == rustix_uid() => {
            fs::remove_file(path)
        }
        Ok(_) => Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "privd socket replacement refused",
        )),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

#[cfg(test)]
mod tests;
