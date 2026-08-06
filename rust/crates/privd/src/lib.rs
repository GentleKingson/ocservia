//! AF_UNIX-only privileged helper exposing fixed, typed Ocserv operations.

#![forbid(unsafe_code)]

use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_agent_protocol::{
    ErrorKind, PrivdError, PrivdRequest, PrivdResponse, privd_request, privd_response, read_frame,
    write_frame,
};
use ocservia_ocserv_adapter::{Adapter, EffectIdentity};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::Semaphore;
use uuid::Uuid;

const MAX_CONCURRENT_CLIENTS: usize = 4;
const MAX_REQUEST_LIFETIME: Duration = Duration::from_secs(30);

/// Privd server configuration.
#[derive(Clone, Debug)]
pub struct ServerConfig {
    /// `AF_UNIX` socket path.
    pub socket: PathBuf,
    /// Only this peer UID may issue requests.
    pub agent_uid: u32,
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
        || parent_metadata.mode() & 0o002 != 0
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
                let Ok(permit) = Arc::clone(&permits).try_acquire_owned() else {
                    tracing::warn!("privd client refused because concurrency is full");
                    continue;
                };
                let adapter = adapter.clone();
                let agent_uid = config.agent_uid;
                tokio::spawn(async move {
                    let _permit = permit;
                    if let Err(error) = handle_client(stream, agent_uid, adapter).await {
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
    let response = dispatch(request, &adapter).await;
    tokio::time::timeout(MAX_REQUEST_LIFETIME, write_frame(&mut stream, &response))
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "privd response write timed out"))??;
    Ok(())
}

async fn dispatch(request: PrivdRequest, adapter: &Adapter) -> PrivdResponse {
    let request_id = request.request_id.clone();
    let result = match validate_request(&request) {
        Ok(deadline) => {
            let effect = EffectIdentity {
                command_id: &request.command_id,
                idempotency_key: &request.idempotency_key,
                semantic_payload_sha256: &request.semantic_payload_sha256,
                expires_at_unix_seconds: request.command_expires_at_unix_seconds,
            };
            match tokio::time::timeout(deadline, execute(request.operation, effect, adapter)).await
            {
                Ok(result) => result,
                Err(_) => privd_response::Result::Error(error(
                    ErrorKind::DeadlineExceeded,
                    "request deadline exceeded",
                )),
            }
        }
        Err(failure) => privd_response::Result::Error(failure),
    };
    PrivdResponse {
        request_id,
        result: Some(result),
    }
}

fn validate_request(request: &PrivdRequest) -> Result<Duration, PrivdError> {
    let id = Uuid::from_slice(&request.request_id)
        .map_err(|_| error(ErrorKind::InvalidRequest, "request_id must be UUIDv7"))?;
    if id.get_version_num() != 7 {
        return Err(error(
            ErrorKind::InvalidRequest,
            "request_id must be UUIDv7",
        ));
    }
    if request.operation.is_none() {
        return Err(error(
            ErrorKind::InvalidRequest,
            "read-only operation required",
        ));
    }
    let desired_operation = matches!(
        request.operation,
        Some(
            privd_request::Operation::DesiredEffectObserve(_)
                | privd_request::Operation::UserCreate(_)
                | privd_request::Operation::UserDisable(_)
                | privd_request::Operation::UserEnable(_)
                | privd_request::Operation::UserPasswordRotate(_)
                | privd_request::Operation::GroupApply(_)
        )
    );
    if desired_operation {
        if request.command_id.len() != 16
            || request.idempotency_key.len() != 16
            || request.semantic_payload_sha256.len() != 32
            || request.command_expires_at_unix_seconds <= 0
        {
            return Err(error(
                ErrorKind::InvalidRequest,
                "desired effect identity invalid",
            ));
        }
    } else if !request.command_id.is_empty()
        || !request.idempotency_key.is_empty()
        || !request.semantic_payload_sha256.is_empty()
        || request.command_expires_at_unix_seconds != 0
    {
        return Err(error(
            ErrorKind::InvalidRequest,
            "effect identity is not allowed for this operation",
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
    Ok(remaining)
}

#[allow(clippy::too_many_lines)]
async fn execute(
    operation: Option<privd_request::Operation>,
    effect: EffectIdentity<'_>,
    adapter: &Adapter,
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
        privd_request::Operation::DesiredEffectObserve(request) => (
            "desired_effect_observe",
            adapter
                .desired_effect_observe(
                    &request.mutation_kind,
                    &request.resource_key,
                    request.desired_revision,
                    effect,
                )
                .await
                .map(privd_response::Result::DesiredEffectObservation),
        ),
        privd_request::Operation::SessionDisconnect(request) => (
            "session_disconnect",
            adapter
                .session_disconnect(&request.session_id, &request.boot_id)
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::SessionTerminate(request) => (
            "session_terminate",
            adapter
                .session_terminate(&request.session_id, &request.boot_id)
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::IpBanRemove(request) => (
            "ip_ban_remove",
            adapter
                .ip_ban_remove(&request.ip)
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::ServiceReload(_) => (
            "service_reload",
            adapter
                .service_reload()
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::UserCreate(request) => (
            "user_create",
            adapter
                .user_create(
                    &request.username,
                    &request.secret_key_id,
                    &request.sealed_password,
                    request.desired_revision,
                    effect,
                )
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::UserDisable(request) => (
            "user_disable",
            adapter
                .user_disable(&request.username, request.desired_revision, effect)
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::UserEnable(request) => (
            "user_enable",
            adapter
                .user_enable(&request.username, request.desired_revision, effect)
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::UserPasswordRotate(request) => (
            "user_password_rotate",
            adapter
                .user_password_rotate(
                    &request.username,
                    &request.secret_key_id,
                    &request.sealed_password,
                    request.desired_revision,
                    effect,
                )
                .await
                .map(privd_response::Result::Mutation),
        ),
        privd_request::Operation::GroupApply(request) => (
            "group_apply",
            adapter
                .group_apply(
                    &request.group_name,
                    &request.members,
                    request.desired_revision,
                    effect,
                )
                .await
                .map(privd_response::Result::Mutation),
        ),
    };
    let result =
        result.unwrap_or_else(|failure| privd_response::Result::Error(PrivdError::from(failure)));
    if let privd_response::Result::Error(mut error) = result {
        error.detail = format!("{operation_name}: {}", error.detail);
        privd_response::Result::Error(error)
    } else {
        result
    }
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
    use super::*;
    use ocservia_agent_protocol::{ReadRequest, privd_request};
    use ocservia_ocserv_adapter::{FixedResources, Limits};

    #[tokio::test]
    async fn rejects_expired_and_missing_operations() {
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        let expired = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: 1,
            command_id: Vec::new(),
            idempotency_key: Vec::new(),
            semantic_payload_sha256: Vec::new(),
            command_expires_at_unix_seconds: 0,
            operation: Some(privd_request::Operation::ServiceStatus(ReadRequest {})),
        };
        let response = dispatch(expired, &adapter).await;
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(_))
        ));

        let missing = PrivdRequest {
            request_id: Uuid::now_v7().as_bytes().to_vec(),
            deadline_unix_ms: u64::MAX,
            command_id: Vec::new(),
            idempotency_key: Vec::new(),
            semantic_payload_sha256: Vec::new(),
            command_expires_at_unix_seconds: 0,
            operation: None,
        };
        let response = dispatch(missing, &adapter).await;
        assert!(matches!(
            response.result,
            Some(privd_response::Result::Error(_))
        ));
    }
}
