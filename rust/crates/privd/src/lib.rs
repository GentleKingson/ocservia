//! AF_UNIX-only privileged helper exposing fixed, typed Ocserv operations.

#![forbid(unsafe_code)]

mod effect_binding;
mod receipts;
mod validation;

use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ed25519_dalek::SigningKey;
use ocservia_agent_protocol::{
    AgentUpgradeScheduledResult, ErrorKind, PrivdError, PrivdRequest, PrivdResponse,
    UpgradeOperationResult, UpgradeResultList, privd_request, privd_response, read_frame,
    write_frame,
};
use ocservia_command_authorization::{CommandAuthorizationV1, ControllerCommandKeyring};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentUpgrade, AgentUpgradeOutcomeState, AgentUpgradeResultProof, CommandEnvelope,
    PrivdReceiptVersion, command_envelope,
};
use ocservia_ocserv_adapter::{Adapter, AdapterError, AuthorizedEffectDecision};
use ocservia_privd_attestation::{key_id, sign_upgrade_result_with_key_id};
use ocservia_upgrader::{ScheduleOutcome, UpgradeIntent, UpgradeScheduler, UpgradeStoreError};
use prost::Message as _;
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::Semaphore;
use uuid::Uuid;

use self::effect_binding::{
    adapter_effect, artifact_identity, authorized_effect, deadline_error, decode_cached_response,
    desired_effect_binding, effect_binding, error, finish_operation, fixed_host_effect,
    owner_fence, replay_only_response, response_error,
};
use self::receipts::{build_result_proof, terminal_result_error_code};
use self::validation::{ValidatedRequest, validate_request};

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
    let attestation_key_id: Arc<str> = Arc::from(key_id(&config.attestation_key.verifying_key()));
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
                let attestation_key_id = Arc::clone(&attestation_key_id);
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
                        attestation_key_id,
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
    attestation_key_id: Arc<str>,
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
        &attestation_key_id,
        &upgrades,
        &adapter,
    )
    .await;
    tokio::time::timeout(MAX_REQUEST_LIFETIME, write_frame(&mut stream, &response))
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd response write timed out"))??;
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn dispatch_attested(
    request: &PrivdRequest,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    attestation_key: &Arc<SigningKey>,
    attestation_key_id: &Arc<str>,
    upgrades: &UpgradeScheduler,
    adapter: &Adapter,
) -> PrivdResponse {
    let request_id = request.request_id.clone();
    let result = match validate_request(request, node_id, endpoint_id, command_keys) {
        Ok((deadline, ValidatedRequest::Read)) => tokio::time::timeout(
            deadline,
            execute_read(
                request.operation.clone(),
                node_id,
                attestation_key,
                attestation_key_id,
                adapter,
                upgrades,
            ),
        )
        .await
        .unwrap_or_else(|_| deadline_error()),
        Ok((deadline, ValidatedRequest::Execute(command, claims, accepted_at, owner_epoch))) => {
            match tokio::time::timeout(
                deadline,
                execute_command(
                    command,
                    &claims,
                    &accepted_at,
                    node_id,
                    endpoint_id,
                    owner_epoch,
                    attestation_key,
                    attestation_key_id,
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
        Ok((deadline, ValidatedRequest::ArtifactRead(claims, offset, owner_epoch))) => {
            match tokio::time::timeout(deadline, async {
                let identity = artifact_identity(&claims, node_id, endpoint_id, owner_epoch);
                adapter.accept_owner_fence(identity.owner_fence)?;
                adapter.artifact_read(identity, offset).await
            })
            .await
            {
                Ok(result) => finish_operation(
                    "artifact_read",
                    result.map(privd_response::Result::ArtifactData),
                ),
                Err(_) => deadline_error(),
            }
        }
        Ok((
            deadline,
            ValidatedRequest::ArtifactConsume(claims, digest, size, confirm_only, owner_epoch),
        )) => {
            match tokio::time::timeout(deadline, async {
                let identity = artifact_identity(&claims, node_id, endpoint_id, owner_epoch);
                adapter.accept_owner_fence(identity.owner_fence)?;
                if confirm_only {
                    adapter
                        .artifact_confirm_consumed(identity, &digest, size)
                        .await
                } else {
                    adapter.artifact_consume(identity, &digest, size).await
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

#[allow(clippy::too_many_lines)]
async fn execute_read(
    operation: Option<privd_request::Operation>,
    node_id: &[u8; 16],
    attestation_key: &Arc<SigningKey>,
    attestation_key_id: &Arc<str>,
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
        privd_request::Operation::UpgradeResultList(_) => {
            let root = upgrades.operations_root().to_path_buf();
            let node_id = *node_id;
            let key = Arc::clone(attestation_key);
            let key_id_str = Arc::clone(attestation_key_id);
            let blocking_result = tokio::task::spawn_blocking(move || {
                read_attested_upgrade_results(&root, &node_id, &key, &key_id_str)
            })
            .await
            .map_err(|join_err| {
                ocservia_ocserv_adapter::AdapterError::Io(io::Error::other(join_err))
            })
            .and_then(|res| {
                res.map_err(|failure| {
                    ocservia_ocserv_adapter::AdapterError::Io(io::Error::other(failure.to_string()))
                })
            });
            ("upgrade_result_list", blocking_result)
        }
        _ => {
            return privd_response::Result::Error(error(
                ErrorKind::PermissionDenied,
                "Controller command authorization required",
            ));
        }
    };
    finish_operation(operation_name, result)
}

fn read_attested_upgrade_results(
    operations_root: &Path,
    node_id: &[u8; 16],
    attestation_key: &SigningKey,
    attestation_key_id: &str,
) -> Result<privd_response::Result, UpgradeStoreError> {
    let results = ocservia_upgrader::read_recent_results(operations_root, 8)?;
    let attested = results
        .into_iter()
        .map(|result| {
            let completed_unix_ms =
                result
                    .completed_unix
                    .checked_mul(1000)
                    .ok_or(UpgradeStoreError::Unsafe(
                        "durable result completion time overflows milliseconds",
                    ))?;
            let state = match result.state {
                ocservia_upgrader::OperationState::Succeeded => AgentUpgradeOutcomeState::Succeeded,
                ocservia_upgrader::OperationState::Failed => AgentUpgradeOutcomeState::Failed,
                ocservia_upgrader::OperationState::RolledBack => {
                    AgentUpgradeOutcomeState::RolledBack
                }
                ocservia_upgrader::OperationState::Accepted
                | ocservia_upgrader::OperationState::Running => {
                    return Err(UpgradeStoreError::Unsafe("durable result is not terminal"));
                }
            };
            // The signature time, not the historical completion
            // time, carries key validity: a key rotated after
            // completion may still attest the older result.
            let attested_unix_ms = u64::try_from(
                SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .map_err(|_| UpgradeStoreError::Unsafe("system clock precedes the unix epoch"))?
                    .as_millis(),
            )
            .map_err(|_| UpgradeStoreError::Unsafe("attestation time overflows milliseconds"))?;
            let proof = sign_upgrade_result_with_key_id(
                AgentUpgradeResultProof {
                    version: PrivdReceiptVersion::V1.into(),
                    node_id: node_id.to_vec(),
                    privd_attestation_key_id: attestation_key_id.to_owned(),
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
                attestation_key_id,
            )
            .map_err(|_| UpgradeStoreError::Unsafe("durable result proof claims are malformed"))?;
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
        .collect::<Result<Vec<_>, UpgradeStoreError>>()?;
    Ok(privd_response::Result::UpgradeResultList(
        UpgradeResultList { results: attested },
    ))
}

#[allow(clippy::too_many_arguments)]
async fn execute_command(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    accepted_at: &prost_types::Timestamp,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    owner_epoch: u64,
    attestation_key: &SigningKey,
    attestation_key_id: &str,
    upgrades: &UpgradeScheduler,
    adapter: &Adapter,
) -> PrivdResponse {
    let owner_fence = owner_fence(node_id, endpoint_id, owner_epoch);
    if let Err(failure) = adapter.accept_owner_fence(owner_fence) {
        return adapter_error_response(failure);
    }
    let binding = effect_binding(command, claims);
    if let Some(binding) = binding.as_ref() {
        let identity = authorized_effect(command, claims, binding);
        // Legacy recovery grants may have renewed an already expired deadline.
        if identity.retry_if_absent
            && (fixed_host_effect(command)
                || command.protocol_version
                    == ocservia_command_authorization::LEGACY_COMMAND_PROTOCOL_VERSION)
        {
            return replay_only_response(adapter, identity, owner_fence);
        }
        match adapter.prepare_authorized_effect(identity, owner_fence) {
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
            Err(failure) => return adapter_error_response(failure),
        }
    }
    let (operation_name, result) =
        if let Some(command_envelope::Payload::AgentUpgrade(payload)) = command.payload.as_ref() {
            (
                "agent_upgrade_prepare",
                execute_agent_upgrade(payload, claims, upgrades).await,
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
        attestation_key_id,
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
        // Concurrent duplicate AgentUpgrade requests are permitted past Pending
        // because the immutable intent store is the idempotency authority; if a
        // racing call already committed the applied receipt, return that
        // canonical HMAC-verified response instead of failing on differing
        // receipt completion timestamps.
        if matches!(
            command.payload.as_ref(),
            Some(command_envelope::Payload::AgentUpgrade(_))
        ) && let Ok(Some(replayed)) =
            adapter.replay_authorized_effect(authorized_effect(command, claims, binding))
        {
            return decode_cached_response(&replayed);
        }
        return adapter_error_response(failure);
    }
    response
}

fn adapter_error_response(failure: AdapterError) -> PrivdResponse {
    PrivdResponse {
        request_id: Vec::new(),
        privileged_result_proof: None,
        result: Some(privd_response::Result::Error(PrivdError::from(failure))),
    }
}

async fn execute_agent_upgrade(
    payload: &AgentUpgrade,
    claims: &CommandAuthorizationV1,
    upgrades: &UpgradeScheduler,
) -> Result<privd_response::Result, ocservia_ocserv_adapter::AdapterError> {
    let payload = payload.clone();
    let operation_id = claims.operation_id;
    let command_id = claims.command_id;
    let semantic_payload_sha256 = claims.semantic_payload_sha256;
    let upgrades = upgrades.clone();
    tokio::task::spawn_blocking(move || {
        schedule_agent_upgrade(
            &payload,
            operation_id,
            command_id,
            semantic_payload_sha256,
            &upgrades,
        )
    })
    .await
    .unwrap_or_else(|join_err| {
        Err(ocservia_ocserv_adapter::AdapterError::Io(io::Error::other(
            join_err,
        )))
    })
}

/// The durable self-upgrade preparation boundary. privd independently
/// verified the Controller-signed release identity, commits the immutable
/// root-owned intent, starts the fixed upgrader unit, and returns only the
/// scheduled acknowledgment. It never executes the upgrade itself, so a
/// normal upgrade cannot destroy the process that owes the command result.
fn schedule_agent_upgrade(
    payload: &AgentUpgrade,
    operation_id: [u8; 16],
    command_id: [u8; 16],
    semantic_payload_sha256: [u8; 32],
    upgrades: &UpgradeScheduler,
) -> Result<privd_response::Result, ocservia_ocserv_adapter::AdapterError> {
    let package_sha256: [u8; 32] = payload
        .package_sha256
        .clone()
        .try_into()
        .map_err(|_| ocservia_ocserv_adapter::AdapterError::InvalidRequest)?;
    let intent = UpgradeIntent::new(
        operation_id,
        command_id,
        &payload.target_version,
        package_sha256,
        &payload.architecture,
        semantic_payload_sha256,
    )
    .map_err(|failure| upgrade_store_error(&failure))?;
    match upgrades.schedule_and_trigger(&intent) {
        Ok(ScheduleOutcome::Scheduled | ScheduleOutcome::AlreadyScheduled) => Ok(
            privd_response::Result::AgentUpgradeScheduled(AgentUpgradeScheduledResult {
                operation_id: operation_id.to_vec(),
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
