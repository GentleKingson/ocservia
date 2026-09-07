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
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, AgentUpgradeOutcomeState, AgentUpgradeResultProof, CommandDeliveryMode,
    CommandEnvelope, CommandResultState, PrivdCertificateReceiptBindingV1, PrivdReceiptVersion,
    PrivdResultReceiptV1, PrivilegedCommandKind, PrivilegedResultKind, SealedSecretPurpose,
    SealedSecretVersion, command_envelope,
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

/// Privd server configuration.
#[derive(Clone)]
pub struct ServerConfig {
    /// `AF_UNIX` socket path.
    pub socket: PathBuf,
    /// Only this peer UID may issue requests.
    pub agent_uid: u32,
    /// Local node UUID pinned independently from the Agent request.
    pub node_id: [u8; 16],
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
                let command_keys = config.command_keys.clone();
                let attestation_key = Arc::clone(&config.attestation_key);
                let upgrades = config.upgrades.clone();
                tokio::spawn(async move {
                    let _permit = permit;
                    if let Err(error) = handle_client(
                        stream,
                        agent_uid,
                        node_id,
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

async fn handle_client(
    mut stream: UnixStream,
    agent_uid: u32,
    node_id: [u8; 16],
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

#[allow(clippy::too_many_lines)]
async fn dispatch_attested(
    request: &PrivdRequest,
    node_id: &[u8; 16],
    command_keys: &ControllerCommandKeyring,
    attestation_key: &SigningKey,
    upgrades: &UpgradeScheduler,
    adapter: &Adapter,
) -> PrivdResponse {
    let request_id = request.request_id.clone();
    let result = match validate_request(request, node_id, command_keys) {
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
        Ok((deadline, ValidatedRequest::Execute(claims, accepted_at))) => {
            let command = request
                .authorization_command
                .clone()
                .expect("validated command must be present");
            match tokio::time::timeout(
                deadline,
                execute_command(
                    &command,
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
        Ok((deadline, ValidatedRequest::Reconcile(claims))) => {
            let command = request
                .authorization_command
                .clone()
                .expect("validated command must be present");
            match tokio::time::timeout(deadline, reconcile_command(&command, &claims, adapter))
                .await
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
    dispatch_attested(&request, node_id, command_keys, &key, upgrades, adapter).await
}

enum ValidatedRequest {
    Read,
    Execute(CommandAuthorizationV1, prost_types::Timestamp),
    Reconcile(CommandAuthorizationV1),
    ArtifactRead(ArtifactGrantClaimsV1, u64),
    ArtifactConsume(ArtifactGrantClaimsV1, Vec<u8>, u64, bool),
}

#[allow(clippy::too_many_lines)]
fn validate_request(
    request: &PrivdRequest,
    node_id: &[u8; 16],
    command_keys: &ControllerCommandKeyring,
) -> Result<(Duration, ValidatedRequest), PrivdError> {
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
                ValidatedRequest::Execute(claims, accepted)
            }
            (PrivilegedRequestMode::Reconcile, CommandDeliveryMode::ReconcileOnly) => {
                if effect_binding(command, &claims).is_none() {
                    return Err(error(
                        ErrorKind::InvalidRequest,
                        "command does not support privileged reconciliation",
                    ));
                }
                ValidatedRequest::Reconcile(claims)
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
        Some(command_envelope::Payload::ConfigPlan(payload)) => {
            validate_candidate(&payload.candidate, &payload.candidate_hash)?;
        }
        Some(command_envelope::Payload::ConfigApply(payload)) => {
            validate_candidate(&payload.candidate, &payload.candidate_hash)?;
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

fn validate_candidate(candidate: &[u8], candidate_hash: &[u8]) -> Result<(), PrivdError> {
    if candidate.is_empty()
        || candidate.len() > 256 * 1024
        || candidate_hash.len() != 32
        || Sha256::digest(candidate).as_slice() != candidate_hash
    {
        return Err(error(
            ErrorKind::InvalidRequest,
            "configuration candidate binding invalid",
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
        command_envelope::Payload::ConfigPlan(payload) => (
            "config_plan",
            hex::encode(&payload.candidate_hash),
            claims.expected_revision,
        ),
        command_envelope::Payload::ConfigApply(payload) => (
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
        command_envelope::Payload::ConfigApply(payload) => (
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
        match adapter.prepare_authorized_effect(authorized_effect(command, claims, binding)) {
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
        Some(command_envelope::Payload::ConfigPlan(payload)) => (
            "config_plan",
            adapter
                .config_plan(&payload.candidate, &payload.candidate_hash)
                .await
                .map(privd_response::Result::ConfigPlan),
        ),
        Some(command_envelope::Payload::ConfigApply(payload)) => (
            "config_apply",
            adapter
                .config_apply(
                    &payload.candidate,
                    &payload.candidate_hash,
                    &payload.expected_current_hash,
                    payload.desired_revision,
                    effect,
                )
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
        command_envelope::Payload::ConfigPlan(_) => Some(PrivilegedCommandKind::ConfigPlan),
        command_envelope::Payload::ConfigApply(_) => Some(PrivilegedCommandKind::ConfigApply),
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
    let result =
        result.unwrap_or_else(|failure| privd_response::Result::Error(PrivdError::from(failure)));
    if let privd_response::Result::Error(mut error) = result {
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
mod tests {
    use std::os::unix::fs::PermissionsExt as _;

    use ed25519_dalek::{Signer as _, SigningKey};
    use ocservia_agent_protocol::{
        ArtifactConsumeRequest, ArtifactReadRequest, CertificateCsrRequest, ConfigApplyRequest,
        ConfigPlanRequest, GroupApplyRequest, IpBanRemoveRequest, ReadRequest,
        ServiceReloadRequest, SessionMutationRequest, UserSecretRequest, privd_request,
    };
    use ocservia_command_authorization::{
        canonical_v1, claims_from_envelope_v1, semantic_payload_hash_v2, verification_key_id,
    };
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        AgentUpgrade, CertificateP12, CommandAuthorizationProof, CommandAuthorizationVersion,
        CommandDeliveryMode, ConfigApply, SealedSecretPurpose, SealedSecretV1, SealedSecretVersion,
        SemanticPayloadHashVersion, ServiceReload, UserCreate, command_envelope,
    };
    use ocservia_ocserv_adapter::{FixedResources, Limits};
    use ocservia_upgrader::UpgradeTrigger;
    use prost_types::Timestamp;

    use super::*;

    fn keyring(key: &SigningKey) -> ControllerCommandKeyring {
        ControllerCommandKeyring::new([key.verifying_key()]).expect("test keyring")
    }

    fn unix_seconds() -> i64 {
        i64::try_from(
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .expect("test clock")
                .as_secs(),
        )
        .expect("test time")
    }

    fn deadline() -> u64 {
        u64::try_from(
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .expect("test clock")
                .as_millis(),
        )
        .expect("test time")
            + 5_000
    }

    fn signed_service_reload(
        key: &SigningKey,
        node_id: [u8; 16],
        issued_at: i64,
        expires_at: i64,
    ) -> CommandEnvelope {
        signed_command(
            key,
            node_id,
            issued_at,
            expires_at,
            "service.reload",
            "ocserv.service.reload",
            command_envelope::Payload::ServiceReload(ServiceReload {}),
        )
    }

    fn signed_agent_upgrade(
        key: &SigningKey,
        node_id: [u8; 16],
        issued_at: i64,
        expires_at: i64,
        target_version: &str,
        package_sha256: Vec<u8>,
        architecture: &str,
    ) -> CommandEnvelope {
        signed_command(
            key,
            node_id,
            issued_at,
            expires_at,
            "agent.upgrade",
            "ocserv.agent.upgrade.v2",
            command_envelope::Payload::AgentUpgrade(AgentUpgrade {
                target_version: target_version.to_owned(),
                package_sha256,
                architecture: architecture.to_owned(),
            }),
        )
    }

    fn host_architecture() -> &'static str {
        ocservia_contracts::agent_upgrade::runtime_architecture().expect("test host architecture")
    }

    fn foreign_architecture() -> &'static str {
        if host_architecture() == "amd64" {
            "arm64"
        } else {
            "amd64"
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn signed_command(
        key: &SigningKey,
        node_id: [u8; 16],
        issued_at: i64,
        expires_at: i64,
        action: &str,
        capability: &str,
        payload: command_envelope::Payload,
    ) -> CommandEnvelope {
        let mut command = CommandEnvelope {
            protocol_version: ocservia_command_authorization::COMMAND_PROTOCOL_VERSION.to_owned(),
            message_id: Uuid::now_v7().as_bytes().to_vec(),
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
            node_id: node_id.to_vec(),
            sequence: 1,
            issued_at: Some(Timestamp {
                seconds: issued_at,
                nanos: 0,
            }),
            expires_at: Some(Timestamp {
                seconds: expires_at,
                nanos: 0,
            }),
            expected_revision: 7,
            actor_id: "controller:test".to_owned(),
            operation_id: Uuid::now_v7().as_bytes().to_vec(),
            action: action.to_owned(),
            required_capability: capability.to_owned(),
            delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
            semantic_payload_hash_version: SemanticPayloadHashVersion::V2.into(),
            payload: Some(payload),
            authorization: Some(CommandAuthorizationProof {
                version: CommandAuthorizationVersion::V1.into(),
                key_id: verification_key_id(&key.verifying_key()),
                signature: Vec::new(),
            }),
            ..CommandEnvelope::default()
        };
        command.semantic_payload_sha256 = semantic_payload_hash_v2(&command)
            .expect("semantic hash")
            .to_vec();
        let claims = claims_from_envelope_v1(&command).expect("authorization claims");
        let signature = key.sign(&canonical_v1(&claims).expect("canonical authorization"));
        command
            .authorization
            .as_mut()
            .expect("authorization")
            .signature = signature.to_bytes().to_vec();
        command
    }

    fn command_request(command: CommandEnvelope) -> PrivdRequest {
        PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: deadline(),
            accepted_at: command.issued_at,
            authorization_command: Some(command),
            privileged_mode: PrivilegedRequestMode::Execute.into(),
            operation: None,
        }
    }

    fn reconcile_request(mut command: CommandEnvelope, key: &SigningKey) -> PrivdRequest {
        command.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
        command.message_id = Uuid::now_v7().as_bytes().to_vec();
        let claims = claims_from_envelope_v1(&command).expect("reconciliation claims");
        command
            .authorization
            .as_mut()
            .expect("reconciliation authorization")
            .signature = key
            .sign(&canonical_v1(&claims).expect("reconciliation canonical bytes"))
            .to_bytes()
            .to_vec();
        PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: deadline(),
            accepted_at: None,
            authorization_command: Some(command),
            privileged_mode: PrivilegedRequestMode::Reconcile.into(),
            operation: None,
        }
    }

    fn unsigned_request(operation: Option<privd_request::Operation>) -> PrivdRequest {
        PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: deadline(),
            accepted_at: None,
            authorization_command: None,
            privileged_mode: PrivilegedRequestMode::Unspecified.into(),
            operation,
        }
    }

    fn test_adapter() -> (Adapter, FixedResources, PathBuf, PathBuf) {
        let directory = PathBuf::from("/tmp").join(format!("ocp-{}", Uuid::now_v7().simple()));
        std::fs::create_dir(&directory).expect("create test directory");
        std::fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))
            .expect("secure test directory");
        let counter = directory.join("reload-count");
        let systemctl = directory.join("systemctl");
        std::fs::write(
            &systemctl,
            format!("#!/bin/sh\nprintf x >> '{}'\n", counter.display()),
        )
        .expect("write fixed test command");
        std::fs::set_permissions(&systemctl, fs::Permissions::from_mode(0o700))
            .expect("make fixed test command executable");
        let config = directory.join("ocserv.conf");
        let boot = directory.join("boot_id");
        std::fs::write(&config, b"fixture\n").expect("write config fixture");
        std::fs::write(&boot, Uuid::now_v7().to_string()).expect("write boot fixture");
        let resources = FixedResources::new(
            systemctl,
            PathBuf::from("/bin/true"),
            PathBuf::from("/bin/true"),
            config,
            boot,
        )
        .expect("fixed resources")
        .with_effect_store(
            directory.join("effects.sqlite3"),
            directory.join("effects.key"),
        )
        .expect("effect resources");
        (
            Adapter::new(resources.clone(), Limits::default()),
            resources,
            counter,
            directory,
        )
    }

    fn assert_permission_denied(response: &PrivdResponse) {
        let Some(privd_response::Result::Error(failure)) = response.result.as_ref() else {
            panic!("privileged request must be rejected")
        };
        assert_eq!(
            ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified),
            ErrorKind::PermissionDenied
        );
    }

    #[tokio::test]
    async fn rejects_expired_and_missing_operations() {
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        let expired = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: 1,
            accepted_at: None,
            authorization_command: None,
            privileged_mode: PrivilegedRequestMode::Unspecified.into(),
            operation: Some(privd_request::Operation::ServiceStatus(ReadRequest {})),
        };
        let response = dispatch(expired, &node_id, &keys, &adapter).await;
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(_))
        ));

        let missing = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: deadline(),
            accepted_at: None,
            authorization_command: None,
            privileged_mode: PrivilegedRequestMode::Unspecified.into(),
            operation: None,
        };
        let response = dispatch(missing, &node_id, &keys, &adapter).await;
        assert_permission_denied(&response);
    }

    #[tokio::test]
    async fn signed_effect_executes_once_and_replays_after_restart() {
        let signing = SigningKey::from_bytes(&[8; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let command = signed_service_reload(&signing, node_id, now, now + 60);
        let (adapter, resources, counter, directory) = test_adapter();

        let first = dispatch(command_request(command.clone()), &node_id, &keys, &adapter).await;
        assert!(matches!(
            first.result,
            Some(privd_response::Result::Mutation(ref result)) if result.applied
        ));
        let proof = first
            .privileged_result_proof
            .clone()
            .expect("successful root effect must be attested");
        ocservia_privd_attestation::verify_receipt(
            &proof,
            &SigningKey::from_bytes(&[41; 32]).verifying_key(),
        )
        .expect("valid root receipt");
        drop(adapter);
        let restarted = Adapter::new(resources, Limits::default());
        let replay = dispatch(
            reconcile_request(command.clone(), &signing),
            &node_id,
            &keys,
            &restarted,
        )
        .await;
        assert!(matches!(
            replay.result,
            Some(privd_response::Result::Mutation(ref result)) if result.applied
        ));
        assert_eq!(replay.privileged_result_proof, Some(proof));
        assert_eq!(replay.result, first.result);
        let duplicate = dispatch(command_request(command), &node_id, &keys, &restarted).await;
        assert_eq!(
            duplicate.privileged_result_proof,
            replay.privileged_result_proof
        );
        assert_eq!(duplicate.result, replay.result);
        assert_eq!(
            std::fs::read_to_string(counter).expect("effect counter"),
            "x"
        );
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test]
    async fn receipt_persistence_failure_cannot_return_privileged_success() {
        let signing = SigningKey::from_bytes(&[42; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let command = signed_service_reload(&signing, node_id, now, now + 60);
        let initializer = signed_service_reload(&signing, node_id, now, now + 60);
        let claims = claims_from_envelope_v1(&initializer).expect("initializer claims");
        let binding = effect_binding(&initializer, &claims).expect("initializer effect binding");
        let (adapter, _resources, counter, directory) = test_adapter();
        assert_eq!(
            adapter
                .prepare_authorized_effect(authorized_effect(&initializer, &claims, &binding))
                .expect("initialize root effect store"),
            AuthorizedEffectDecision::Execute
        );
        {
            let connection = rusqlite::Connection::open(directory.join("effects.sqlite3"))
                .expect("open root effect store for fault injection");
            connection
                .execute_batch(
                    "CREATE TRIGGER fail_attested_response BEFORE UPDATE OF response ON authorized_effects BEGIN SELECT RAISE(ABORT, 'injected receipt persistence failure'); END;",
                )
                .expect("install completion fault");
        }

        let response = dispatch(command_request(command), &node_id, &keys, &adapter).await;
        assert!(response.privileged_result_proof.is_none());
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(ref error))
                if ErrorKind::try_from(error.kind).ok() == Some(ErrorKind::Unavailable)
        ));
        assert_eq!(
            std::fs::read_to_string(counter).expect("effect counter"),
            "x",
            "the completed side effect must remain Unknown rather than be repeated"
        );
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test]
    async fn signed_claim_or_effect_tampering_is_rejected_before_root_effect() {
        let signing = SigningKey::from_bytes(&[9; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let command = signed_service_reload(&signing, node_id, now, now + 60);
        let (adapter, _, counter, directory) = test_adapter();
        let mut variants = Vec::new();

        let mut changed = command.clone();
        changed.node_id = Uuid::now_v7().as_bytes().to_vec();
        variants.push(changed);
        let mut changed = command.clone();
        changed.expected_revision += 1;
        variants.push(changed);
        let mut changed = command.clone();
        changed.command_id = Uuid::now_v7().as_bytes().to_vec();
        variants.push(changed);
        let mut changed = command.clone();
        changed.idempotency_key = Uuid::now_v7().as_bytes().to_vec();
        variants.push(changed);
        let mut changed = command.clone();
        changed.required_capability = "ocserv.config.apply".to_owned();
        variants.push(changed);
        let mut changed = command.clone();
        changed.semantic_payload_sha256[0] ^= 1;
        variants.push(changed);
        let mut changed = command.clone();
        changed.operation_id = Uuid::now_v7().as_bytes().to_vec();
        variants.push(changed);
        let mut changed = command.clone();
        changed.actor_id = "forged-controller".to_owned();
        variants.push(changed);
        let mut changed = command.clone();
        changed.payload = Some(command_envelope::Payload::IpBanRemove(
            ocservia_contracts::generated::ocserv::platform::agent::v1::IpBanRemove {
                ip: "192.0.2.1".to_owned(),
            },
        ));
        variants.push(changed);
        let mut changed = command.clone();
        changed
            .authorization
            .as_mut()
            .expect("authorization")
            .signature[0] ^= 1;
        variants.push(changed);

        for changed in variants {
            let response = dispatch(command_request(changed), &node_id, &keys, &adapter).await;
            assert_permission_denied(&response);
        }
        let mut agent_selected_effect = command_request(command);
        agent_selected_effect.operation =
            Some(privd_request::Operation::IpBanRemove(IpBanRemoveRequest {
                ip: "192.0.2.1".to_owned(),
            }));
        let response = dispatch(agent_selected_effect, &node_id, &keys, &adapter).await;
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(ref failure))
                if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                    == ErrorKind::InvalidRequest
        ));
        assert!(!counter.exists());
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    fn upgrade_scheduler(directory: &Path) -> (UpgradeScheduler, PathBuf) {
        let operations_dir = directory.join("upgrade-operations");
        fs::create_dir(&operations_dir).expect("create upgrade operations root");
        fs::set_permissions(&operations_dir, fs::Permissions::from_mode(0o700))
            .expect("secure upgrade operations root");
        (
            UpgradeScheduler::new(operations_dir.clone(), UpgradeTrigger::Disabled),
            operations_dir,
        )
    }

    fn durable_operation_state(operations_dir: &Path, operation_id: &[u8]) -> String {
        let id = Uuid::from_slice(operation_id).expect("operation UUID");
        let state = fs::read_to_string(operations_dir.join(id.to_string()).join("state"))
            .expect("durable operation state");
        state.trim_end_matches('\n').to_owned()
    }

    #[tokio::test]
    async fn agent_upgrade_prepare_attests_scheduled_intent_exactly_once() {
        let signing = SigningKey::from_bytes(&[29; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let command = signed_agent_upgrade(
            &signing,
            node_id,
            now,
            now + 60,
            "1.2.3",
            vec![0x43; 32],
            host_architecture(),
        );
        let (adapter, resources, _, directory) = test_adapter();
        let (upgrades, operations_dir) = upgrade_scheduler(&directory);

        let first = dispatch_upgrade(
            command_request(command.clone()),
            &node_id,
            &keys,
            &adapter,
            &upgrades,
        )
        .await;
        let Some(privd_response::Result::AgentUpgradeScheduled(ref scheduled)) = first.result
        else {
            panic!("upgrade preparation must return the scheduled intent");
        };
        assert_eq!(scheduled.target_version, "1.2.3");
        assert_eq!(scheduled.package_sha256, vec![0x43; 32]);
        assert_eq!(scheduled.operation_id, command.operation_id);
        // The durable root-owned intent outlives both privd and the Agent.
        assert_eq!(
            durable_operation_state(&operations_dir, &command.operation_id),
            "accepted"
        );
        assert!(
            operations_dir
                .join(
                    Uuid::from_slice(&command.operation_id)
                        .expect("operation UUID")
                        .to_string()
                )
                .join("intent")
                .exists()
        );
        let proof = first
            .privileged_result_proof
            .clone()
            .expect("scheduled intent must be root-attested");
        ocservia_privd_attestation::verify_receipt(
            &proof,
            &SigningKey::from_bytes(&[41; 32]).verifying_key(),
        )
        .expect("valid scheduled-intent receipt");
        let receipt = proof.receipt_v1.as_ref().expect("receipt");
        assert_eq!(
            PrivilegedCommandKind::try_from(receipt.command_kind),
            Ok(PrivilegedCommandKind::AgentUpgrade)
        );
        assert_eq!(
            PrivilegedResultKind::try_from(receipt.result_kind),
            Ok(PrivilegedResultKind::AgentUpgradeScheduled)
        );
        assert_eq!(
            CommandResultState::try_from(receipt.terminal_state),
            Ok(CommandResultState::Succeeded)
        );

        let duplicate = dispatch_upgrade(
            command_request(command.clone()),
            &node_id,
            &keys,
            &adapter,
            &upgrades,
        )
        .await;
        assert_eq!(duplicate.privileged_result_proof, Some(proof.clone()));
        assert_eq!(duplicate.result, first.result);
        assert_eq!(
            durable_operation_state(&operations_dir, &command.operation_id),
            "accepted"
        );
        drop(adapter);
        let restarted = Adapter::new(resources, Limits::default());
        let replay = dispatch_upgrade(
            reconcile_request(command, &signing),
            &node_id,
            &keys,
            &restarted,
            &upgrades,
        )
        .await;
        assert_eq!(replay.privileged_result_proof, Some(proof));
        assert_eq!(replay.result, first.result);
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test]
    async fn agent_upgrade_pending_journal_replays_while_runner_holds_execution_lock() {
        use std::fs::OpenOptions;
        use std::os::unix::fs::OpenOptionsExt as _;

        let signing = SigningKey::from_bytes(&[33; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let command = signed_agent_upgrade(
            &signing,
            node_id,
            now,
            now + 60,
            "1.2.3",
            vec![0x61; 32],
            host_architecture(),
        );
        let claims = claims_from_envelope_v1(&command).expect("upgrade claims");
        let binding = effect_binding(&command, &claims).expect("upgrade effect binding");
        let (adapter, _, _, directory) = test_adapter();
        assert_eq!(
            adapter
                .prepare_authorized_effect(authorized_effect(&command, &claims, &binding))
                .expect("prepare pending root effect"),
            AuthorizedEffectDecision::Execute
        );
        let (upgrades, operations_dir) = upgrade_scheduler(&directory);
        let Some(command_envelope::Payload::AgentUpgrade(payload)) = command.payload.as_ref()
        else {
            panic!("upgrade payload");
        };
        let intent = UpgradeIntent::new(
            claims.operation_id,
            claims.command_id,
            &payload.target_version,
            payload
                .package_sha256
                .clone()
                .try_into()
                .expect("package digest"),
            &payload.architecture,
            claims.semantic_payload_sha256,
        )
        .expect("upgrade intent");
        assert_eq!(
            upgrades
                .schedule_and_trigger(&intent)
                .expect("commit durable intent"),
            ScheduleOutcome::Scheduled
        );
        let durable_path = operations_dir.join(intent.operation_id.to_string());
        fs::write(durable_path.join("state"), b"running\n").expect("runner state");
        let execution_lock = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(operations_dir.join(".run.lock"))
            .expect("execution lock file");
        rustix::fs::flock(
            &execution_lock,
            rustix::fs::FlockOperation::NonBlockingLockExclusive,
        )
        .expect("hold runner execution lock");

        let replay = dispatch_upgrade(
            command_request(command.clone()),
            &node_id,
            &keys,
            &adapter,
            &upgrades,
        )
        .await;
        assert!(matches!(
            replay.result,
            Some(privd_response::Result::AgentUpgradeScheduled(_))
        ));
        let receipt = replay
            .privileged_result_proof
            .as_ref()
            .and_then(|proof| proof.receipt_v1.as_ref())
            .expect("successful replay receipt");
        assert_eq!(
            CommandResultState::try_from(receipt.terminal_state),
            Ok(CommandResultState::Succeeded)
        );
        assert_eq!(
            durable_operation_state(&operations_dir, &command.operation_id),
            "running"
        );
        drop(execution_lock);
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn concurrent_duplicate_agent_upgrades_both_attest_the_schedule() {
        let signing = SigningKey::from_bytes(&[34; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let command = signed_agent_upgrade(
            &signing,
            node_id,
            now,
            now + 60,
            "1.2.3",
            vec![0x71; 32],
            host_architecture(),
        );
        let operation_id = command.operation_id.clone();
        let (adapter, _, _, directory) = test_adapter();
        let (upgrades, operations_dir) = upgrade_scheduler(&directory);

        // Two identical deliveries race through the Pending journal and the
        // durable schedule lock. The loser must observe the committed intent
        // as an exact replay - never a terminal privd_rejected receipt.
        let (first, second) = tokio::join!(
            dispatch_upgrade(
                command_request(command.clone()),
                &node_id,
                &keys,
                &adapter,
                &upgrades
            ),
            dispatch_upgrade(
                command_request(command),
                &node_id,
                &keys,
                &adapter,
                &upgrades
            )
        );
        for response in [&first, &second] {
            assert!(
                matches!(
                    response.result,
                    Some(privd_response::Result::AgentUpgradeScheduled(_))
                ),
                "concurrent duplicate must replay the scheduled intent, got {:?}",
                response.result
            );
            let receipt = response
                .privileged_result_proof
                .as_ref()
                .and_then(|proof| proof.receipt_v1.as_ref())
                .expect("successful scheduled-intent receipt");
            assert_eq!(
                CommandResultState::try_from(receipt.terminal_state),
                Ok(CommandResultState::Succeeded)
            );
        }
        let durable = fs::read_dir(&operations_dir)
            .expect("operations root")
            .filter_map(Result::ok)
            .filter(|entry| Uuid::parse_str(&entry.file_name().to_string_lossy()).is_ok())
            .count();
        assert_eq!(durable, 1);
        assert_eq!(
            durable_operation_state(&operations_dir, &operation_id),
            "accepted"
        );
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test]
    async fn agent_upgrade_prepare_refuses_a_second_active_operation() {
        let signing = SigningKey::from_bytes(&[32; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let first_command = signed_agent_upgrade(
            &signing,
            node_id,
            now,
            now + 60,
            "1.2.3",
            vec![0x51; 32],
            host_architecture(),
        );
        let second_command = signed_agent_upgrade(
            &signing,
            node_id,
            now,
            now + 60,
            "1.2.4",
            vec![0x52; 32],
            host_architecture(),
        );
        let (adapter, _, _, directory) = test_adapter();
        let (upgrades, operations_dir) = upgrade_scheduler(&directory);

        let first = dispatch_upgrade(
            command_request(first_command),
            &node_id,
            &keys,
            &adapter,
            &upgrades,
        )
        .await;
        assert!(matches!(
            first.result,
            Some(privd_response::Result::AgentUpgradeScheduled(_))
        ));
        let second = dispatch_upgrade(
            command_request(second_command),
            &node_id,
            &keys,
            &adapter,
            &upgrades,
        )
        .await;
        assert!(matches!(
            second.result,
            Some(privd_response::Result::Error(ref failure))
                if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                    == ErrorKind::InvalidRequest
        ));
        // The refused command still receives a terminal failure receipt.
        if let Some(proof) = second.privileged_result_proof.as_ref() {
            assert_eq!(
                CommandResultState::try_from(
                    proof.receipt_v1.as_ref().expect("receipt").terminal_state
                ),
                Ok(CommandResultState::Failed)
            );
        }
        let durable: Vec<_> = std::fs::read_dir(&operations_dir)
            .expect("list durable operations")
            .filter_map(Result::ok)
            .filter(|entry| Uuid::parse_str(&entry.file_name().to_string_lossy()).is_ok())
            .collect();
        assert_eq!(durable.len(), 1, "only the first operation may be durable");
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test]
    async fn agent_upgrade_prepare_rejects_unverified_release_identities() {
        let signing = SigningKey::from_bytes(&[30; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let adapter = Adapter::new(FixedResources::default(), Limits::default());

        // A foreign-but-well-formed release identity passes the semantic hash
        // and Controller signature, so it must be refused by privd's own
        // host-architecture validation, not by an earlier generic failure.
        let foreign = signed_agent_upgrade(
            &signing,
            node_id,
            now,
            now + 60,
            "1.2.3",
            vec![0x43; 32],
            foreign_architecture(),
        );
        let response = dispatch(command_request(foreign), &node_id, &keys, &adapter).await;
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(ref failure))
                if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                    == ErrorKind::InvalidRequest
        ));

        // Unknown controller keys never reach the release identity checks.
        let unknown_key = SigningKey::from_bytes(&[31; 32]);
        let forged = signed_agent_upgrade(
            &unknown_key,
            node_id,
            now,
            now + 60,
            "1.2.3",
            vec![0x43; 32],
            host_architecture(),
        );
        let response = dispatch(command_request(forged), &node_id, &keys, &adapter).await;
        assert_permission_denied(&response);

        // Unsigned upgrade intents have no fixed local operation to ride on.
        let response = dispatch(unsigned_request(None), &node_id, &keys, &adapter).await;
        assert_permission_denied(&response);
    }

    #[tokio::test]
    async fn cross_purpose_password_envelopes_fail_before_root_effect() {
        let signing = SigningKey::from_bytes(&[19; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        let user_secret = SealedSecretV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::UserPassword as i32,
            key_id: "user-key-v1".to_owned(),
            ciphertext: vec![0xa5; 256],
        };
        let mut user_command = signed_command(
            &signing,
            node_id,
            now,
            now + 60,
            "user.create",
            "ocserv.users.write",
            command_envelope::Payload::UserCreate(UserCreate {
                username: "alice".to_owned(),
                sealed_password: Vec::new(),
                secret_key_id: String::new(),
                desired_revision: 1,
                sealed_password_v1: Some(user_secret),
            }),
        );
        let Some(command_envelope::Payload::UserCreate(payload)) = user_command.payload.as_mut()
        else {
            unreachable!()
        };
        payload
            .sealed_password_v1
            .as_mut()
            .expect("typed user secret")
            .purpose = SealedSecretPurpose::CertificateP12Password as i32;
        let response = dispatch(command_request(user_command), &node_id, &keys, &adapter).await;
        assert_permission_denied(&response);

        let p12_secret = SealedSecretV1 {
            version: SealedSecretVersion::V1 as i32,
            purpose: SealedSecretPurpose::CertificateP12Password as i32,
            key_id: "p12-key-v1".to_owned(),
            ciphertext: vec![0x5a; 256],
        };
        let mut p12_command = signed_command(
            &signing,
            node_id,
            now,
            now + 60,
            "certificate.private_key.export",
            "ocserv.certificate.issue",
            command_envelope::Payload::CertificateP12(CertificateP12 {
                certificate_id: Uuid::now_v7().as_bytes().to_vec(),
                certificate_chain_pem: vec![b'A'; 64],
                sealed_password: Vec::new(),
                secret_key_id: String::new(),
                artifact_id: Uuid::now_v7().as_bytes().to_vec(),
                sealed_password_v1: Some(p12_secret),
                certificate_version: 1,
                artifact_expires_at: Some(Timestamp {
                    seconds: now + 60,
                    nanos: 0,
                }),
            }),
        );
        let Some(command_envelope::Payload::CertificateP12(payload)) = p12_command.payload.as_mut()
        else {
            unreachable!()
        };
        payload
            .sealed_password_v1
            .as_mut()
            .expect("typed P12 secret")
            .purpose = SealedSecretPurpose::UserPassword as i32;
        let response = dispatch(command_request(p12_command), &node_id, &keys, &adapter).await;
        assert_permission_denied(&response);
    }

    #[tokio::test]
    async fn signed_config_hash_cannot_authorize_substituted_candidate_bytes() {
        let signing = SigningKey::from_bytes(&[14; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let candidate = b"# generated by ocservia config-plan/v1\ntcp-port = 443\n".to_vec();
        let candidate_hash = Sha256::digest(&candidate).to_vec();
        let mut command = signed_command(
            &signing,
            node_id,
            now,
            now + 60,
            "config.apply",
            "ocserv.config.apply",
            command_envelope::Payload::ConfigApply(ConfigApply {
                candidate,
                candidate_hash,
                expected_current_hash: vec![0x44; 32],
                desired_revision: 2,
            }),
        );
        let Some(command_envelope::Payload::ConfigApply(payload)) = command.payload.as_mut() else {
            panic!("config payload")
        };
        payload.candidate[0] ^= 1;
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        let response = dispatch(command_request(command), &node_id, &keys, &adapter).await;
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(ref failure))
                if ErrorKind::try_from(failure.kind).unwrap_or(ErrorKind::Unspecified)
                    == ErrorKind::InvalidRequest
        ));
    }

    #[tokio::test]
    async fn expired_and_unknown_controller_proofs_are_rejected() {
        let trusted = SigningKey::from_bytes(&[10; 32]);
        let unknown = SigningKey::from_bytes(&[11; 32]);
        let keys = keyring(&trusted);
        let node_id = *Uuid::now_v7().as_bytes();
        let now = unix_seconds();
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        let expired = signed_service_reload(&trusted, node_id, now - 60, now - 1);
        assert_permission_denied(
            &dispatch(command_request(expired), &node_id, &keys, &adapter).await,
        );
        let another_node =
            signed_service_reload(&trusted, *Uuid::now_v7().as_bytes(), now, now + 60);
        assert_permission_denied(
            &dispatch(command_request(another_node), &node_id, &keys, &adapter).await,
        );
        let unknown = signed_service_reload(&unknown, node_id, now, now + 60);
        assert_permission_denied(
            &dispatch(command_request(unknown), &node_id, &keys, &adapter).await,
        );
    }

    #[tokio::test]
    async fn every_unsigned_legacy_mutation_family_fails_closed() {
        let signing = SigningKey::from_bytes(&[12; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        let mutations = vec![
            privd_request::Operation::UserPasswordRotate(UserSecretRequest::default()),
            privd_request::Operation::GroupApply(GroupApplyRequest::default()),
            privd_request::Operation::ConfigPlan(ConfigPlanRequest::default()),
            privd_request::Operation::ConfigApply(ConfigApplyRequest::default()),
            privd_request::Operation::CertificateCsr(CertificateCsrRequest::default()),
            privd_request::Operation::SessionDisconnect(SessionMutationRequest::default()),
            privd_request::Operation::IpBanRemove(IpBanRemoveRequest::default()),
            privd_request::Operation::ServiceReload(ServiceReloadRequest {}),
            privd_request::Operation::ArtifactRead(ArtifactReadRequest::default()),
            privd_request::Operation::ArtifactConsume(ArtifactConsumeRequest::default()),
        ];
        for mutation in mutations {
            let response =
                dispatch(unsigned_request(Some(mutation)), &node_id, &keys, &adapter).await;
            assert_permission_denied(&response);
        }
    }

    #[tokio::test]
    async fn socket_binding_requires_trusted_parent_and_preserves_socket_access() {
        use std::os::unix::fs::{chown, symlink};

        let directory =
            PathBuf::from("/tmp").join(format!("ocp-socket-{}", Uuid::now_v7().simple()));
        fs::create_dir(&directory).expect("test directory");
        let signing = SigningKey::from_bytes(&[42; 32]);
        let mut config = ServerConfig {
            socket: directory.join("privd.sock"),
            agent_uid: rustix_uid(),
            node_id: *Uuid::now_v7().as_bytes(),
            command_keys: keyring(&signing),
            attestation_key: Arc::new(signing),
            upgrades: UpgradeScheduler::disabled(),
        };
        for mode in [0o755, 0o750] {
            fs::set_permissions(&directory, fs::Permissions::from_mode(mode)).unwrap();
            let listener = bind_socket(&config).expect("trusted parent");
            assert_eq!(fs::metadata(&config.socket).unwrap().mode() & 0o777, 0o660);
            drop(listener);
        }
        remove_socket(&config.socket).unwrap();
        for mode in [0o775, 0o757, 0o777] {
            fs::set_permissions(&directory, fs::Permissions::from_mode(mode)).unwrap();
            assert_eq!(
                bind_socket(&config).unwrap_err().kind(),
                io::ErrorKind::PermissionDenied
            );
            assert!(!config.socket.exists());
        }
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o755)).unwrap();
        // Changing ownership requires root; the BuildServer regression runs as root.
        if rustix_uid() == 0 {
            chown(&directory, Some(65534), None).unwrap();
            let result = bind_socket(&config);
            chown(&directory, Some(0), None).unwrap();
            assert_eq!(result.unwrap_err().kind(), io::ErrorKind::PermissionDenied);
        }
        let alias = directory.with_extension("link");
        symlink(&directory, &alias).unwrap();
        config.socket = alias.join("privd.sock");
        assert_eq!(
            bind_socket(&config).unwrap_err().kind(),
            io::ErrorKind::PermissionDenied
        );
        fs::remove_file(alias).unwrap();
        config.socket = directory.join("privd.sock");
        fs::write(&config.socket, b"do not replace").unwrap();
        assert_eq!(
            bind_socket(&config).unwrap_err().kind(),
            io::ErrorKind::AlreadyExists
        );
        assert_eq!(fs::read(&config.socket).unwrap(), b"do not replace");
        fs::remove_file(&config.socket).unwrap();
        symlink(directory.join("missing"), &config.socket).unwrap();
        assert_eq!(
            bind_socket(&config).unwrap_err().kind(),
            io::ErrorKind::AlreadyExists
        );
        fs::remove_file(&config.socket).unwrap();
        fs::remove_dir(directory).unwrap();
    }

    #[tokio::test]
    async fn saturated_clients_wait_for_capacity_instead_of_being_reset() {
        let signing = SigningKey::from_bytes(&[42; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let (adapter, _, _, directory) = test_adapter();
        let socket = directory.join("privd.sock");
        let config = ServerConfig {
            socket: socket.clone(),
            agent_uid: rustix_uid(),
            node_id,
            command_keys: keys,
            attestation_key: Arc::new(SigningKey::from_bytes(&[43; 32])),
            upgrades: UpgradeScheduler::disabled(),
        };
        let listener = bind_socket(&config).expect("bind privd fixture");
        let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();
        let server_config = config.clone();
        let server = tokio::spawn(async move {
            serve(listener, server_config, adapter, async {
                let _ = shutdown_rx.await;
            })
            .await
        });

        // Keep every active slot inside the bounded request reader. A valid
        // request arriving next must wait for one slot rather than being
        // accepted and reset merely because cleanup of the prior batch has
        // not released its permit yet.
        let mut held = Vec::with_capacity(MAX_CONCURRENT_CLIENTS);
        for _ in 0..MAX_CONCURRENT_CLIENTS {
            held.push(
                UnixStream::connect(&socket)
                    .await
                    .expect("connect held privd client"),
            );
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        let mut queued = UnixStream::connect(&socket)
            .await
            .expect("connect queued privd client");
        let request = unsigned_request(Some(privd_request::Operation::ServiceStatus(
            ReadRequest {},
        )));
        let request_id = request.request_id.clone();
        write_frame(&mut queued, &request)
            .await
            .expect("write queued read request");
        let response = tokio::spawn(async move {
            let response: Result<PrivdResponse, io::Error> = read_frame(&mut queued).await;
            response
        });
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(
            !response.is_finished(),
            "a saturated but valid local client was reset instead of queued"
        );

        drop(held.pop());
        let response = tokio::time::timeout(Duration::from_secs(2), response)
            .await
            .expect("queued request remained blocked after capacity returned")
            .expect("join queued request")
            .expect("read queued response");
        assert_eq!(response.request_id, request_id);
        assert!(response.result.is_some());

        drop(held);
        shutdown_tx.send(()).expect("stop server");
        server.await.expect("join server").expect("serve fixture");
        remove_socket(&socket).expect("remove fixture socket");
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test]
    async fn same_uid_direct_socket_caller_cannot_forge_root_effect() {
        let signing = SigningKey::from_bytes(&[13; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let (adapter, _, counter, directory) = test_adapter();
        let socket = directory.join("privd.sock");
        let config = ServerConfig {
            socket: socket.clone(),
            agent_uid: rustix_uid(),
            node_id,
            command_keys: keys,
            attestation_key: Arc::new(SigningKey::from_bytes(&[41; 32])),
            upgrades: UpgradeScheduler::disabled(),
        };
        let listener = bind_socket(&config).expect("bind privd fixture");
        let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();
        let server_config = config.clone();
        let server = tokio::spawn(async move {
            serve(listener, server_config, adapter, async {
                let _ = shutdown_rx.await;
            })
            .await
        });
        let mut stream = UnixStream::connect(&socket)
            .await
            .expect("same UID connect");
        let request = unsigned_request(Some(privd_request::Operation::ServiceReload(
            ServiceReloadRequest {},
        )));
        write_frame(&mut stream, &request)
            .await
            .expect("write forged request");
        let response: PrivdResponse = read_frame(&mut stream).await.expect("read rejection");
        assert_permission_denied(&response);
        assert!(!counter.exists());
        shutdown_tx.send(()).expect("stop server");
        server.await.expect("join server").expect("serve fixture");
        remove_socket(&socket).expect("remove fixture socket");
        std::fs::remove_dir_all(directory).expect("cleanup test directory");
    }

    #[tokio::test]
    async fn upgrade_result_list_surfaces_durable_outcomes_read_only() {
        let signing = SigningKey::from_bytes(&[7; 32]);
        let keys = keyring(&signing);
        let node_id = *Uuid::now_v7().as_bytes();
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        let directory =
            std::env::temp_dir().join(format!("ocservia-privd-upgrade-read-{}", Uuid::now_v7()));
        fs::create_dir(&directory).expect("create test directory");
        let operations = directory.join("var/lib/ocservia-upgrade/operations");
        fs::create_dir_all(&operations).expect("create operations root");
        let scheduler = UpgradeScheduler::new(operations.clone(), UpgradeTrigger::Disabled);
        let intent = UpgradeIntent::new(
            *Uuid::now_v7().as_bytes(),
            *Uuid::now_v7().as_bytes(),
            "2.0.0",
            [0x43; 32],
            ocservia_contracts::agent_upgrade::runtime_architecture().expect("host architecture"),
            [0x55; 32],
        )
        .expect("valid test intent");
        scheduler
            .schedule_and_trigger(&intent)
            .expect("schedule intent");
        // The spool is empty, so the runner durably refuses the upgrade and
        // leaves exactly the terminal evidence the read-only query must show.
        let runner = ocservia_upgrader::UpgradeRunner::new(directory.clone());
        assert!(runner.run(&intent.operation_id.to_string()).is_err());

        let request = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: deadline(),
            accepted_at: None,
            authorization_command: None,
            privileged_mode: PrivilegedRequestMode::Unspecified.into(),
            operation: Some(privd_request::Operation::UpgradeResultList(
                ocservia_agent_protocol::UpgradeResultListRequest {},
            )),
        };
        let response = dispatch_attested(
            &request,
            &node_id,
            &keys,
            &SigningKey::from_bytes(&[41; 32]),
            &scheduler,
            &adapter,
        )
        .await;
        let Some(privd_response::Result::UpgradeResultList(list)) = response.result else {
            panic!("expected an upgrade result list, got {:?}", response.result);
        };
        assert_eq!(list.results.len(), 1);
        assert_eq!(list.results[0].operation_id, intent.operation_id.as_bytes());
        assert_eq!(list.results[0].state, "failed");
        assert_eq!(list.results[0].target_version, "2.0.0");
        assert!(list.results[0].completed_unix_ms > 0);
        let proof = list.results[0]
            .privileged_result_proof
            .as_ref()
            .expect("root-attested durable result");
        assert_eq!(proof.node_id, node_id);
        assert_eq!(proof.operation_id, intent.operation_id.as_bytes());
        assert_eq!(proof.package_sha256, intent.package_sha256);
        assert_eq!(proof.result_sha256.len(), 32);
        assert_eq!(proof.signature.len(), ed25519_dalek::SIGNATURE_LENGTH);
        std::fs::remove_dir_all(&directory).expect("cleanup test directory");
    }
}
