use std::env;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::net::UnixStream as StdUnixStream;
use std::path::{Component, Path, PathBuf};
use std::sync::Arc;

use iroh::endpoint::RelayMode;
use iroh::{RelayMap, RelayUrl, SecretKey};
use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_server::TransportServiceServer;
use ocservia_transportd::{
    IdentityPolicy, IrohTransportService, TrustAuthority, TrustBudgetConfig, TrustClass,
    TrustClassLimit, build_router_with_trust_tls_roots,
};
use rustls_pki_types::pem::PemObject;
use tokio::net::UnixListener;
use tokio_stream::StreamExt;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;
use zeroize::Zeroizing;

#[derive(Debug)]
struct Config {
    socket: PathBuf,
    key_file: PathBuf,
    relay_mode: RelayMode,
    event_capacity: usize,
    trust_socket: PathBuf,
    control_plane_uid: u32,
    control_plane_gid: u32,
    trust_budgets: TrustBudgetConfig,
    controller_verification_key_files: Vec<PathBuf>,
    stats_file: Option<PathBuf>,
    relay_ca_file: Option<PathBuf>,
}

#[tokio::main]
#[allow(clippy::similar_names, clippy::too_many_lines)]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,iroh=warn".into()),
        )
        .init();
    let config = parse_args()?;
    if let Some(stats_file) = config.stats_file.clone() {
        ocservia_observability::spawn_runtime_stats_writer(stats_file)?;
    }
    let key = load_key(&config.key_file)?;
    let (listener, socket_identity) = bind_socket(&config.socket)?;
    let keyring = load_controller_keyring(&config)?;
    let relay_tls_roots = load_relay_tls_roots(&config)?;
    let policy = IdentityPolicy::default();
    let service = IrohTransportService::new(config.event_capacity, policy.clone(), keyring);
    let trust_socket = config.trust_socket;
    let control_plane_uid = config.control_plane_uid;
    let control_plane_gid = config.control_plane_gid;
    let channel = tonic::transport::Endpoint::try_from("http://[::]:50051")?
        .connect_with_connector(tower::service_fn(move |_| {
            let path = trust_socket.clone();
            async move {
                connect_verified_socket(&path, control_plane_uid, control_plane_gid)
                    .await
                    .map(hyper_util::rt::TokioIo::new)
            }
        }))
        .await?;
    let router = build_router_with_trust_tls_roots(
        key,
        config.relay_mode,
        policy,
        TrustAuthority::new_with_config(channel, &config.trust_budgets),
        relay_tls_roots,
        &service,
    )
    .await?;
    tracing::info!(
        endpoint_id = %router.endpoint().id(),
        socket = %config.socket.display(),
        "transportd serving"
    );
    let (health_reporter, health_service) = tonic_health::server::health_reporter();
    health_reporter
        .set_serving::<TransportServiceServer<IrohTransportService>>()
        .await;
    let shutdown_service = service.clone();
    let control_plane_uid = config.control_plane_uid;
    let incoming =
        UnixListenerStream::new(listener).filter_map(move |connection| match connection {
            Ok(stream) => match authenticate_peer(&stream, control_plane_uid) {
                Ok(()) => Some(Ok(stream)),
                Err(error) => {
                    tracing::warn!(%error, "rejected transport UDS peer");
                    None
                }
            },
            Err(error) => Some(Err(error)),
        });
    let result = Server::builder()
        .add_service(health_service)
        .add_service(TransportServiceServer::new(service.clone()))
        .serve_with_incoming_shutdown(incoming, async move {
            shutdown_signal().await;
            shutdown_service.begin_shutdown().await;
        })
        .await;
    health_reporter
        .set_not_serving::<TransportServiceServer<IrohTransportService>>()
        .await;
    let router_result = ocservia_transportd::shutdown(&service, router).await;
    let socket_result = remove_socket(&config.socket, socket_identity);
    result?;
    router_result?;
    socket_result?;
    Ok(())
}

#[allow(clippy::similar_names, clippy::too_many_lines)]
fn parse_args() -> Result<Config, io::Error> {
    let mut socket = PathBuf::from("/run/ocserv-platform/transportd.sock");
    let mut key_file = None;
    let mut relay_mode = String::from("disabled");
    let mut relay_url: Option<String> = None;
    let mut relay_token_file = None;
    let mut event_capacity = 256_usize;
    let mut trust_socket = None;
    let mut control_plane_uid = None;
    let mut control_plane_gid = None;
    let mut controller_verification_key_files = Vec::new();
    let mut stats_file = None;
    let mut relay_ca_file = None;
    let mut trust_limits = [
        (
            TrustClass::KnownAgentHandshake,
            TrustClassLimit {
                attempts_per_minute: 2400,
                concurrency: 256,
            },
        ),
        (
            TrustClass::AgentAuthorization,
            TrustClassLimit {
                attempts_per_minute: 1200,
                concurrency: 48,
            },
        ),
        (
            TrustClass::AgentRegistrationRecheck,
            TrustClassLimit {
                attempts_per_minute: 1200,
                concurrency: 48,
            },
        ),
        (
            TrustClass::UnknownEnrollmentPreAuth,
            TrustClassLimit {
                attempts_per_minute: 120,
                concurrency: 16,
            },
        ),
        (
            TrustClass::EnrollmentCompletion,
            TrustClassLimit {
                attempts_per_minute: 120,
                concurrency: 8,
            },
        ),
    ];
    let mut args = env::args().skip(1);
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--socket" => socket = PathBuf::from(required_value(&mut args, "--socket")?),
            "--key-file" => {
                key_file = Some(PathBuf::from(required_value(&mut args, "--key-file")?));
            }
            "--relay-mode" => {
                relay_mode = required_value(&mut args, "--relay-mode")?;
            }
            "--relay-url" => {
                if relay_url.is_some() {
                    return Err(invalid("only one --relay-url is allowed"));
                }
                relay_url = Some(required_value(&mut args, "--relay-url")?);
            }
            "--relay-token-file" => {
                relay_token_file = Some(PathBuf::from(required_value(
                    &mut args,
                    "--relay-token-file",
                )?));
            }
            "--event-capacity" => {
                event_capacity = required_value(&mut args, "--event-capacity")?
                    .parse()
                    .map_err(|_| invalid("event capacity must be an integer"))?;
            }
            "--trust-socket" => {
                trust_socket = Some(PathBuf::from(required_value(&mut args, "--trust-socket")?));
            }
            "--control-plane-uid" => {
                control_plane_uid = Some(parse_u32(
                    &required_value(&mut args, "--control-plane-uid")?,
                    "control-plane UID",
                )?);
            }
            "--control-plane-gid" => {
                control_plane_gid = Some(parse_u32(
                    &required_value(&mut args, "--control-plane-gid")?,
                    "control-plane GID",
                )?);
            }
            "--controller-verification-key-file" => {
                controller_verification_key_files.push(PathBuf::from(required_value(
                    &mut args,
                    "--controller-verification-key-file",
                )?));
            }
            "--known-agent-handshake-rate" => {
                trust_limits[0].1.attempts_per_minute = parse_usize(
                    &required_value(&mut args, "--known-agent-handshake-rate")?,
                    "known Agent handshake rate",
                )?;
            }
            "--known-agent-handshake-concurrency" => {
                trust_limits[0].1.concurrency = parse_usize(
                    &required_value(&mut args, "--known-agent-handshake-concurrency")?,
                    "known Agent handshake concurrency",
                )?;
            }
            "--agent-authorization-rate" => {
                trust_limits[1].1.attempts_per_minute = parse_usize(
                    &required_value(&mut args, "--agent-authorization-rate")?,
                    "Agent authorization rate",
                )?;
            }
            "--agent-authorization-concurrency" => {
                trust_limits[1].1.concurrency = parse_usize(
                    &required_value(&mut args, "--agent-authorization-concurrency")?,
                    "Agent authorization concurrency",
                )?;
            }
            "--agent-registration-recheck-rate" => {
                trust_limits[2].1.attempts_per_minute = parse_usize(
                    &required_value(&mut args, "--agent-registration-recheck-rate")?,
                    "Agent registration recheck rate",
                )?;
            }
            "--agent-registration-recheck-concurrency" => {
                trust_limits[2].1.concurrency = parse_usize(
                    &required_value(&mut args, "--agent-registration-recheck-concurrency")?,
                    "Agent registration recheck concurrency",
                )?;
            }
            "--unknown-enrollment-preauth-rate" => {
                trust_limits[3].1.attempts_per_minute = parse_usize(
                    &required_value(&mut args, "--unknown-enrollment-preauth-rate")?,
                    "unknown enrollment pre-auth rate",
                )?;
            }
            "--unknown-enrollment-preauth-concurrency" => {
                trust_limits[3].1.concurrency = parse_usize(
                    &required_value(&mut args, "--unknown-enrollment-preauth-concurrency")?,
                    "unknown enrollment pre-auth concurrency",
                )?;
            }
            "--enrollment-completion-rate" => {
                trust_limits[4].1.attempts_per_minute = parse_usize(
                    &required_value(&mut args, "--enrollment-completion-rate")?,
                    "enrollment completion rate",
                )?;
            }
            "--enrollment-completion-concurrency" => {
                trust_limits[4].1.concurrency = parse_usize(
                    &required_value(&mut args, "--enrollment-completion-concurrency")?,
                    "enrollment completion concurrency",
                )?;
            }
            "--stats-file" => {
                stats_file = Some(PathBuf::from(required_value(&mut args, "--stats-file")?));
            }
            "--relay-ca-file" => {
                let path = PathBuf::from(required_value(&mut args, "--relay-ca-file")?);
                if !path.is_absolute() {
                    return Err(invalid("--relay-ca-file must be an absolute path"));
                }
                relay_ca_file = Some(path);
            }
            _ => return Err(invalid(&format!("unknown argument: {argument}"))),
        }
    }
    let key_file = key_file.ok_or_else(|| invalid("--key-file is required"))?;
    let trust_socket = trust_socket.ok_or_else(|| invalid("--trust-socket is required"))?;
    let control_plane_uid =
        control_plane_uid.ok_or_else(|| invalid("--control-plane-uid is required"))?;
    let control_plane_gid =
        control_plane_gid.ok_or_else(|| invalid("--control-plane-gid is required"))?;
    if !socket.is_absolute()
        || !key_file.is_absolute()
        || !trust_socket.is_absolute()
        || relay_token_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
        || controller_verification_key_files
            .iter()
            .any(|path| !path.is_absolute())
    {
        return Err(invalid("socket and key file paths must be absolute"));
    }
    if event_capacity == 0 || event_capacity > 4096 {
        return Err(invalid("event capacity must be 1..4096"));
    }
    if controller_verification_key_files.is_empty() || controller_verification_key_files.len() > 8 {
        return Err(invalid(
            "1..8 --controller-verification-key-file paths are required",
        ));
    }
    if stats_file.as_ref().is_some_and(|path| !path.is_absolute()) {
        return Err(invalid("stats file path must be absolute"));
    }
    let relay_mode = build_relay_mode(
        &relay_mode,
        relay_url.as_deref(),
        relay_token_file.as_deref(),
    )?;
    let mut trust_budgets = TrustBudgetConfig::default();
    for (class, limit) in trust_limits {
        trust_budgets = trust_budgets.with_limit(class, limit).map_err(invalid)?;
    }
    Ok(Config {
        socket,
        key_file,
        relay_mode,
        event_capacity,
        trust_socket,
        control_plane_uid,
        control_plane_gid,
        trust_budgets,
        controller_verification_key_files,
        stats_file,
        relay_ca_file,
    })
}

/// Loads the PEM relay certificate authority as additional TLS roots for the
/// relay client. Deployments with publicly certificated relays leave this
/// unset and keep the built-in root store unchanged.
fn load_relay_tls_roots(
    config: &Config,
) -> Result<Vec<rustls_pki_types::CertificateDer<'static>>, io::Error> {
    let Some(path) = config.relay_ca_file.as_ref() else {
        return Ok(Vec::new());
    };
    let roots: Vec<_> = rustls_pki_types::CertificateDer::pem_file_iter(path)
        .map_err(|error| invalid(&format!("relay CA file is unreadable: {error}")))?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| invalid(&format!("relay CA PEM is invalid: {error}")))?;
    if roots.is_empty() {
        return Err(invalid("relay CA file contains no certificates"));
    }
    Ok(roots)
}

/// Loads the Controller command verification keys through the same safe path
/// walk used for the transport key, pinned to the control-plane account that
/// publishes them.
fn load_controller_keyring(
    config: &Config,
) -> Result<Arc<ocservia_command_authorization::ControllerCommandKeyring>, io::Error> {
    let keys = config
        .controller_verification_key_files
        .iter()
        .map(|path| {
            ocservia_command_authorization::load_verification_key(
                path,
                config.control_plane_uid,
                config.control_plane_gid,
            )
        })
        .collect::<Result<Vec<_>, _>>()?;
    let keyring =
        ocservia_command_authorization::ControllerCommandKeyring::new(keys).map_err(|error| {
            invalid(&format!(
                "controller verification keys are invalid: {error}"
            ))
        })?;
    Ok(Arc::new(keyring))
}

fn parse_usize(value: &str, name: &str) -> Result<usize, io::Error> {
    value
        .parse()
        .map_err(|_| invalid(&format!("{name} must be a positive integer")))
}

fn parse_u32(value: &str, name: &str) -> Result<u32, io::Error> {
    value
        .parse::<u32>()
        .map_err(|_| invalid(&format!("{name} must be an unsigned 32-bit integer")))
}

fn build_relay_mode(
    mode: &str,
    raw_url: Option<&str>,
    token_file: Option<&Path>,
) -> Result<RelayMode, io::Error> {
    match mode {
        "disabled" if raw_url.is_none() && token_file.is_none() => Ok(RelayMode::Disabled),
        "disabled" => Err(invalid(
            "relay URL and token are accepted only with custom relay mode",
        )),
        "custom" => {
            let raw = raw_url.ok_or_else(|| invalid("custom relay mode requires --relay-url"))?;
            let token_file = token_file
                .ok_or_else(|| invalid("custom relay mode requires --relay-token-file"))?;
            let url: RelayUrl = raw.parse().map_err(|_| invalid("relay URL is invalid"))?;
            if url.scheme() != "https"
                || !url.username().is_empty()
                || url.password().is_some()
                || url.query().is_some()
                || url.fragment().is_some()
                || url.host_str().is_none()
            {
                return Err(invalid(
                    "relay URL must be credential-free HTTPS without query or fragment",
                ));
            }
            let token = read_relay_token(token_file)?;
            Ok(RelayMode::Custom(
                RelayMap::from_iter([url]).with_auth_token(token),
            ))
        }
        _ => Err(invalid("relay mode must be disabled or custom")),
    }
}

fn read_relay_token(path: &Path) -> Result<String, io::Error> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)?;
    validate_relay_token_file(
        &file,
        rustix::process::geteuid().as_raw(),
        rustix::process::getegid().as_raw(),
    )?;
    let mut raw = Vec::with_capacity(129);
    std::io::Read::read_to_end(&mut std::io::Read::take(&mut file, 513), &mut raw)?;
    let raw = Zeroizing::new(raw);
    let token = std::str::from_utf8(&raw)
        .map_err(|_| invalid("relay token must be UTF-8"))?
        .trim_end_matches(['\n', '\r']);
    if !(32..=512).contains(&token.len()) || token.chars().any(char::is_whitespace) {
        return Err(invalid(
            "relay token must be 32..512 non-whitespace UTF-8 bytes",
        ));
    }
    Ok(token.to_owned())
}

// The Controller and transport share only the Relay bearer token. Endpoint
// identity keys continue through validate_key_file's owner-only checks.
fn validate_relay_token_file(
    file: &std::fs::File,
    expected_owner: u32,
    expected_group: u32,
) -> Result<(), io::Error> {
    let metadata = file.metadata()?;
    let owner_only = metadata.mode().trailing_zeros() >= 6;
    let controller_readable = metadata.gid() == expected_group && metadata.mode() & 0o7777 == 0o440;
    if !metadata.file_type().is_file()
        || metadata.nlink() != 1
        || metadata.uid() != expected_owner
        || !(owner_only || controller_readable)
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "relay token must be a single-link process-owned file with owner-only permissions or mode 0440 in the process group",
        ));
    }
    Ok(())
}

fn required_value(
    args: &mut impl Iterator<Item = String>,
    name: &'static str,
) -> Result<String, io::Error> {
    args.next()
        .ok_or_else(|| invalid(&format!("{name} requires a value")))
}

fn load_key(path: &Path) -> Result<SecretKey, io::Error> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)?;
    validate_key_file(&file, rustix::process::geteuid().as_raw())?;
    let mut raw = Vec::with_capacity(65);
    std::io::Read::read_to_end(&mut file, &mut raw)?;
    let bytes = Zeroizing::new(raw);
    let mut secret = Zeroizing::new([0_u8; 32]);
    if bytes.len() == 32 {
        secret.copy_from_slice(&bytes);
    } else {
        let text = std::str::from_utf8(&bytes)
            .map_err(|_| invalid("key file must contain 32 raw bytes or 64 lowercase hex"))?
            .trim_end_matches(['\n', '\r']);
        if text.len() != 64 || text.bytes().any(|byte| byte.is_ascii_uppercase()) {
            return Err(invalid(
                "key file must contain 32 raw bytes or 64 lowercase hex",
            ));
        }
        let decoded = Zeroizing::new(
            hex::decode(text).map_err(|_| invalid("key file contains invalid hex"))?,
        );
        if decoded.len() != secret.len() {
            return Err(invalid("key file must contain exactly 32 bytes"));
        }
        secret.copy_from_slice(&decoded);
    }
    let key = SecretKey::from_bytes(&secret);
    Ok(key)
}

fn validate_key_file(file: &std::fs::File, expected_uid: u32) -> Result<(), io::Error> {
    let metadata = file.metadata()?;
    if !metadata.file_type().is_file() {
        return Err(invalid("key path must be a regular file, not a symlink"));
    }
    if metadata.uid() != expected_uid || metadata.mode() & 0o077 != 0 {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "key file must be owned by the process user with mode 0600 or stricter",
        ));
    }
    Ok(())
}

#[allow(clippy::cast_possible_wrap)]
const fn libc_o_nofollow() -> i32 {
    rustix::fs::OFlags::NOFOLLOW.bits() as i32
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct SocketIdentity {
    device: u64,
    inode: u64,
}

fn bind_socket(path: &Path) -> Result<(UnixListener, SocketIdentity), io::Error> {
    validate_ancestry(path, rustix::process::geteuid().as_raw())?;
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_socket() => {
            validate_socket(
                path,
                rustix::process::geteuid().as_raw(),
                rustix::process::getegid().as_raw(),
            )?;
            match StdUnixStream::connect(path) {
                Ok(_) => {
                    return Err(io::Error::new(
                        io::ErrorKind::AlreadyExists,
                        "socket is already accepting connections",
                    ));
                }
                Err(error) if error.kind() == io::ErrorKind::ConnectionRefused => {
                    std::fs::remove_file(path)?;
                }
                Err(error) => return Err(error),
            }
        }
        Ok(_) => {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "refusing to replace a non-socket path",
            ));
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let listener = UnixListener::bind(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o660))?;
    let identity = validate_socket(
        path,
        rustix::process::geteuid().as_raw(),
        rustix::process::getegid().as_raw(),
    )?;
    Ok((listener, identity))
}

fn validate_ancestry(path: &Path, expected_owner: u32) -> Result<(), io::Error> {
    if !path.is_absolute()
        || path
            .components()
            .any(|component| !matches!(component, Component::RootDir | Component::Normal(_)))
    {
        return Err(invalid("Unix socket path must be absolute and normalized"));
    }
    let mut current = path
        .parent()
        .ok_or_else(|| invalid("socket path has no parent"))?;
    loop {
        let metadata = std::fs::symlink_metadata(current)?;
        if !metadata.file_type().is_dir()
            || (metadata.uid() != 0 && metadata.uid() != expected_owner)
            || metadata.mode() & 0o022 != 0
        {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "Unix socket ancestry must be trusted-owner controlled and not group or world writable",
            ));
        }
        let Some(parent) = current.parent() else {
            break;
        };
        if parent == current {
            break;
        }
        current = parent;
    }
    Ok(())
}

#[allow(clippy::similar_names)]
fn validate_socket(
    path: &Path,
    expected_uid: u32,
    expected_gid: u32,
) -> Result<SocketIdentity, io::Error> {
    validate_ancestry(path, expected_uid)?;
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.file_type().is_socket()
        || metadata.uid() != expected_uid
        || metadata.gid() != expected_gid
        || metadata.mode() & 0o777 != 0o660
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "Unix socket type, owner, group, or mode is invalid",
        ));
    }
    Ok(SocketIdentity {
        device: metadata.dev(),
        inode: metadata.ino(),
    })
}

#[allow(clippy::similar_names)]
async fn connect_verified_socket(
    path: &Path,
    expected_uid: u32,
    expected_gid: u32,
) -> Result<tokio::net::UnixStream, io::Error> {
    let identity = validate_socket(path, expected_uid, expected_gid)?;
    let stream = tokio::net::UnixStream::connect(path).await?;
    let credentials = stream.peer_cred()?;
    if credentials.uid() != expected_uid {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "Unix socket server UID is not authorized",
        ));
    }
    let retained = validate_socket(path, expected_uid, expected_gid)?;
    if retained != identity {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "Unix socket pathname changed while connecting",
        ));
    }
    Ok(stream)
}

fn authenticate_peer(stream: &tokio::net::UnixStream, expected_uid: u32) -> Result<(), io::Error> {
    let credentials = stream.peer_cred()?;
    if credentials.uid() != expected_uid {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "Unix socket client UID is not authorized",
        ));
    }
    Ok(())
}

fn remove_socket(path: &Path, expected: SocketIdentity) -> Result<(), io::Error> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata)
            if metadata.file_type().is_socket()
                && metadata.dev() == expected.device
                && metadata.ino() == expected.inode =>
        {
            std::fs::remove_file(path)
        }
        Ok(_) => Err(io::Error::other("socket path changed during shutdown")),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.to_owned())
}

async fn shutdown_signal() {
    let terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate());
    let Ok(mut terminate) = terminate else {
        tracing::error!("failed to install SIGTERM handler");
        let _ = tokio::signal::ctrl_c().await;
        return;
    };
    tokio::select! {
        result = tokio::signal::ctrl_c() => {
            if result.is_err() {
                tracing::error!("failed to install Ctrl-C handler");
            }
        }
        _ = terminate.recv() => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn socket_test_directory() -> PathBuf {
        static NEXT_DIRECTORY: AtomicUsize = AtomicUsize::new(0);
        std::env::current_dir()
            .expect("current test directory")
            .join(format!(
                ".s-{}-{}",
                std::process::id(),
                NEXT_DIRECTORY.fetch_add(1, Ordering::Relaxed)
            ))
    }

    #[test]
    fn key_loader_rejects_world_readable_file() {
        let directory = std::env::temp_dir().join(format!("ocservia-key-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("create test directory");
        let path = directory.join("controller.key");
        let mut file = std::fs::File::create(&path).expect("create key");
        file.write_all(&[7_u8; 32]).expect("write key");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644))
            .expect("set permissions");
        assert_eq!(
            load_key(&path).expect_err("insecure key rejected").kind(),
            io::ErrorKind::PermissionDenied
        );
        std::fs::remove_dir_all(directory).expect("remove test directory");
    }

    #[test]
    fn key_validator_rejects_a_different_owner() {
        let directory = std::env::temp_dir().join(format!("ocservia-key-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("create test directory");
        let path = directory.join("controller.key");
        let file = std::fs::File::create(&path).expect("create key");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))
            .expect("set permissions");
        let different_uid = file.metadata().expect("key metadata").uid().wrapping_add(1);
        assert_eq!(
            validate_key_file(&file, different_uid)
                .expect_err("foreign-owned key rejected")
                .kind(),
            io::ErrorKind::PermissionDenied
        );
        std::fs::remove_dir_all(directory).expect("remove test directory");
    }

    #[test]
    fn custom_relay_requires_single_https_url_and_token() {
        let directory =
            std::env::temp_dir().join(format!("relay-contract-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("test directory");
        let token = directory.join("token");
        std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("token");
        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600)).expect("mode");

        let RelayMode::Custom(map) =
            build_relay_mode("custom", Some("https://relay.example.test"), Some(&token))
                .expect("valid single dedicated relay")
        else {
            panic!("custom mode required");
        };
        assert_eq!(map.len(), 1);
        assert!(build_relay_mode("custom", None, Some(&token)).is_err());

        for invalid_url in [
            "",
            "not a URL",
            "http://relay.example.test",
            "https://user:pass@relay.example.test",
            "https://relay.example.test?token=x",
            "https://relay.example.test#fragment",
            "https://bad host",
            "https://user\"@relay.example.test",
            "https://$(touch /tmp/relay-injection)",
        ] {
            assert!(
                build_relay_mode("custom", Some(invalid_url), Some(&token)).is_err(),
                "accepted {invalid_url:?}"
            );
        }
        let url = "https://relay.example.test";
        assert!(build_relay_mode("custom", Some(url), None).is_err());
        assert!(build_relay_mode("custom", Some(url), Some(&directory.join("missing"))).is_err());
        assert!(build_relay_mode("disabled", None, None).is_ok());
        assert!(build_relay_mode("disabled", Some(url), None).is_err());
        assert!(build_relay_mode("disabled", None, Some(&token)).is_err());
        assert!(build_relay_mode("default", None, None).is_err());

        for invalid_token in ["short", "0123456789abcdef 123456789abcdef0"] {
            std::fs::write(&token, invalid_token).expect("invalid token");
            assert!(build_relay_mode("custom", Some(url), Some(&token)).is_err());
        }
        std::fs::write(&token, "0123456789abcdef0123456789abcdef").expect("valid token");
        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644)).expect("mode");
        assert!(build_relay_mode("custom", Some(url), Some(&token)).is_err());
        std::fs::remove_dir_all(directory).expect("cleanup");
    }

    #[test]
    fn relay_token_sharing_does_not_relax_identity_key_permissions() {
        let directory =
            std::env::temp_dir().join(format!("ocservia-relay-sharing-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("create directory");
        let path = directory.join("token");
        std::fs::write(&path, "0123456789abcdef0123456789abcdef").expect("write token");
        let file = std::fs::File::open(&path).expect("open token");
        let metadata = file.metadata().expect("metadata");
        let (uid, gid) = (metadata.uid(), metadata.gid());
        for mode in [0o400, 0o600, 0o440] {
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode)).unwrap();
            assert!(validate_relay_token_file(&file, uid, gid).is_ok());
        }
        assert!(validate_key_file(&file, uid).is_err());
        assert!(validate_relay_token_file(&file, uid.wrapping_add(1), gid).is_err());
        assert!(validate_relay_token_file(&file, uid, gid.wrapping_add(1)).is_err());
        for mode in [0o640, 0o444, 0o644, 0o460] {
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode)).unwrap();
            assert!(validate_relay_token_file(&file, uid, gid).is_err());
        }
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o440)).unwrap();
        std::fs::hard_link(&path, directory.join("alias")).unwrap();
        assert!(validate_relay_token_file(&file, uid, gid).is_err());
        std::fs::remove_dir_all(directory).unwrap();
    }

    #[tokio::test]
    async fn second_instance_cannot_replace_a_live_socket() {
        let directory = socket_test_directory();
        std::fs::create_dir(&directory).expect("create test directory");
        let path = directory.join("s");
        let (listener, identity) = bind_socket(&path).expect("bind first socket");

        assert_eq!(
            bind_socket(&path)
                .expect_err("live socket must not be replaced")
                .kind(),
            io::ErrorKind::AlreadyExists
        );
        let metadata = std::fs::symlink_metadata(&path).expect("socket metadata");
        assert_eq!(
            (metadata.dev(), metadata.ino()),
            (identity.device, identity.inode)
        );

        drop(listener);
        remove_socket(&path, identity).expect("remove first socket");
        std::fs::remove_dir(directory).expect("remove test directory");
    }

    #[tokio::test]
    async fn shutdown_does_not_unlink_a_replacement_socket() {
        let directory = socket_test_directory();
        std::fs::create_dir(&directory).expect("create test directory");
        let path = directory.join("s");
        let (first_listener, first_identity) = bind_socket(&path).expect("bind first socket");
        std::fs::remove_file(&path).expect("remove first socket path");
        let (replacement_listener, replacement_identity) =
            bind_socket(&path).expect("bind replacement socket");

        assert_eq!(
            remove_socket(&path, first_identity)
                .expect_err("replacement socket must be preserved")
                .kind(),
            io::ErrorKind::Other
        );
        assert!(path.exists());

        drop(first_listener);
        drop(replacement_listener);
        remove_socket(&path, replacement_identity).expect("remove replacement socket");
        std::fs::remove_dir(directory).expect("remove test directory");
    }

    #[tokio::test]
    async fn transport_server_rejects_a_foreign_peer_uid() {
        let directory = socket_test_directory();
        std::fs::create_dir(&directory).expect("create test directory");
        std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
            .expect("protect test directory");
        let path = directory.join("s");
        let (listener, identity) = bind_socket(&path).expect("bind socket");
        let client = StdUnixStream::connect(&path).expect("connect socket");
        let (server, _) = listener.accept().await.expect("accept socket");
        let foreign_uid = rustix::process::geteuid().as_raw().wrapping_add(1);
        assert_eq!(
            authenticate_peer(&server, foreign_uid)
                .expect_err("foreign peer rejected")
                .kind(),
            io::ErrorKind::PermissionDenied
        );
        drop(client);
        drop(server);
        drop(listener);
        remove_socket(&path, identity).expect("remove socket");
        std::fs::remove_dir(directory).expect("remove test directory");
    }

    #[tokio::test]
    async fn trust_client_rejects_a_foreign_owned_socket() {
        let directory = socket_test_directory();
        std::fs::create_dir(&directory).expect("create test directory");
        std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))
            .expect("protect test directory");
        let path = directory.join("s");
        let (listener, identity) = bind_socket(&path).expect("bind fake trust socket");
        let foreign_uid = rustix::process::geteuid().as_raw().wrapping_add(1);
        assert_eq!(
            connect_verified_socket(&path, foreign_uid, rustix::process::getegid().as_raw(),)
                .await
                .expect_err("foreign-owned trust socket rejected")
                .kind(),
            io::ErrorKind::PermissionDenied
        );
        drop(listener);
        remove_socket(&path, identity).expect("remove socket");
        std::fs::remove_dir(directory).expect("remove test directory");
    }

    #[test]
    fn nofollow_flag_matches_platform_and_rejects_symlinks() {
        assert_eq!(
            libc_o_nofollow(),
            i32::try_from(rustix::fs::OFlags::NOFOLLOW.bits()).expect("NOFOLLOW fits in i32")
        );
        let directory = socket_test_directory();
        std::fs::create_dir(&directory).expect("create test directory");
        let real_key = directory.join("real.key");
        std::fs::write(&real_key, [0x42_u8; 32]).expect("write real key");
        std::fs::set_permissions(&real_key, std::fs::Permissions::from_mode(0o600))
            .expect("protect real key");
        let link_key = directory.join("link.key");
        std::os::unix::fs::symlink(&real_key, &link_key).expect("symlink key");
        assert!(
            load_key(&link_key).is_err(),
            "symlinked key must be rejected"
        );

        let real_token = directory.join("real.token");
        std::fs::write(&real_token, "0123456789abcdef0123456789abcdef").expect("write real token");
        std::fs::set_permissions(&real_token, std::fs::Permissions::from_mode(0o600))
            .expect("protect real token");
        let link_token = directory.join("link.token");
        std::os::unix::fs::symlink(&real_token, &link_token).expect("symlink token");
        assert!(
            read_relay_token(&link_token).is_err(),
            "symlinked relay token must be rejected"
        );
        std::fs::remove_dir_all(directory).expect("remove test directory");
    }
}
