use std::collections::{HashMap, HashSet};
use std::env;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::net::UnixStream as StdUnixStream;
use std::path::{Component, Path, PathBuf};

use iroh::endpoint::RelayMode;
use iroh::{EndpointId, RelayMap, RelayUrl, SecretKey};
use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_server::TransportServiceServer;
use ocservia_transportd::{
    IdentityPolicy, IrohTransportService, TrustAuthority, build_router, build_router_with_trust,
    spawn_dedicated_relay_failover,
};
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
    approved: HashMap<EndpointId, Vec<u8>>,
    revoked: HashSet<EndpointId>,
    event_capacity: usize,
    trust_socket: Option<PathBuf>,
    control_plane_uid: u32,
    control_plane_gid: u32,
}

#[tokio::main]
#[allow(clippy::similar_names)]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,iroh=warn".into()),
        )
        .init();
    let config = parse_args()?;
    let relay_failover = match &config.relay_mode {
        RelayMode::Custom(relays) => Some(relays.clone()),
        _ => None,
    };
    let key = load_key(&config.key_file)?;
    let (listener, socket_identity) = bind_socket(&config.socket)?;
    let policy = IdentityPolicy::new(config.approved, config.revoked);
    let service = IrohTransportService::new_with_policy(config.event_capacity, policy.clone());
    let router = if let Some(trust_socket) = config.trust_socket.clone() {
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
        build_router_with_trust(
            key,
            config.relay_mode,
            policy,
            TrustAuthority::new(channel),
            &service,
        )
        .await?
    } else {
        build_router(key, config.relay_mode, policy, &service).await?
    };
    tracing::info!(
        endpoint_id = %router.endpoint().id(),
        socket = %config.socket.display(),
        "transportd serving"
    );
    if let Some(relays) = relay_failover {
        spawn_dedicated_relay_failover(router.endpoint().clone(), relays);
    }

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

#[allow(clippy::similar_names)]
fn parse_args() -> Result<Config, io::Error> {
    let mut socket = PathBuf::from("/run/ocserv-platform/transportd.sock");
    let mut key_file = None;
    let mut relay_mode = String::from("default");
    let mut relay_urls = Vec::new();
    let mut relay_token_file = None;
    let mut approved = HashMap::new();
    let mut revoked = HashSet::new();
    let mut event_capacity = 256_usize;
    let mut trust_socket = None;
    let mut control_plane_uid = None;
    let mut control_plane_gid = None;
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
                relay_urls.push(required_value(&mut args, "--relay-url")?);
            }
            "--relay-token-file" => {
                relay_token_file = Some(PathBuf::from(required_value(
                    &mut args,
                    "--relay-token-file",
                )?));
            }
            "--approved-binding" => {
                let (endpoint, node_id) =
                    parse_binding(&required_value(&mut args, "--approved-binding")?)?;
                if approved.insert(endpoint, node_id).is_some() {
                    return Err(invalid("approved endpoint is bound more than once"));
                }
            }
            "--revoked-endpoint" => {
                revoked.insert(parse_endpoint(&required_value(
                    &mut args,
                    "--revoked-endpoint",
                )?)?);
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
            _ => return Err(invalid(&format!("unknown argument: {argument}"))),
        }
    }
    let key_file = key_file.ok_or_else(|| invalid("--key-file is required"))?;
    let control_plane_uid =
        control_plane_uid.ok_or_else(|| invalid("--control-plane-uid is required"))?;
    let control_plane_gid =
        control_plane_gid.ok_or_else(|| invalid("--control-plane-gid is required"))?;
    if !socket.is_absolute()
        || !key_file.is_absolute()
        || relay_token_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
        || trust_socket
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
    {
        return Err(invalid("socket and key file paths must be absolute"));
    }
    if event_capacity == 0 || event_capacity > 4096 {
        return Err(invalid("event capacity must be 1..4096"));
    }
    let relay_mode = build_relay_mode(&relay_mode, relay_urls, relay_token_file.as_deref())?;
    Ok(Config {
        socket,
        key_file,
        relay_mode,
        approved,
        revoked,
        event_capacity,
        trust_socket,
        control_plane_uid,
        control_plane_gid,
    })
}

fn parse_u32(value: &str, name: &str) -> Result<u32, io::Error> {
    value
        .parse::<u32>()
        .map_err(|_| invalid(&format!("{name} must be an unsigned 32-bit integer")))
}

fn build_relay_mode(
    mode: &str,
    raw_urls: Vec<String>,
    token_file: Option<&Path>,
) -> Result<RelayMode, io::Error> {
    match mode {
        "default" if raw_urls.is_empty() && token_file.is_none() => Ok(RelayMode::Default),
        "disabled" if raw_urls.is_empty() && token_file.is_none() => Ok(RelayMode::Disabled),
        "default" | "disabled" => Err(invalid(
            "relay URLs and token are accepted only with custom relay mode",
        )),
        "custom" => {
            if !(2..=8).contains(&raw_urls.len()) {
                return Err(invalid("custom relay mode requires 2..8 relay URLs"));
            }
            let token_file = token_file
                .ok_or_else(|| invalid("custom relay mode requires --relay-token-file"))?;
            let mut urls = Vec::with_capacity(raw_urls.len());
            for raw in raw_urls {
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
                if urls.contains(&url) {
                    return Err(invalid("relay URLs must be unique"));
                }
                urls.push(url);
            }
            let token = read_relay_token(token_file)?;
            Ok(RelayMode::Custom(
                RelayMap::from_iter(urls).with_auth_token(token),
            ))
        }
        _ => Err(invalid("relay mode must be default, disabled, or custom")),
    }
}

fn read_relay_token(path: &Path) -> Result<String, io::Error> {
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)?;
    validate_key_file(&file, rustix::process::geteuid().as_raw()).map_err(|_| {
        io::Error::new(
            io::ErrorKind::PermissionDenied,
            "relay token must be owned by the process user with mode 0600 or stricter",
        )
    })?;
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

fn required_value(
    args: &mut impl Iterator<Item = String>,
    name: &'static str,
) -> Result<String, io::Error> {
    args.next()
        .ok_or_else(|| invalid(&format!("{name} requires a value")))
}

fn parse_endpoint(value: &str) -> Result<EndpointId, io::Error> {
    let bytes = hex::decode(value).map_err(|_| invalid("endpoint ID must be lowercase hex"))?;
    if bytes.len() != 32 || value.bytes().any(|byte| byte.is_ascii_uppercase()) {
        return Err(invalid("endpoint ID must be 32-byte lowercase hex"));
    }
    let bytes: [u8; 32] = bytes
        .try_into()
        .map_err(|_| invalid("endpoint ID must be 32 bytes"))?;
    EndpointId::from_bytes(&bytes).map_err(|_| invalid("endpoint ID is invalid"))
}

fn parse_binding(value: &str) -> Result<(EndpointId, Vec<u8>), io::Error> {
    let (node_id, endpoint) = value
        .split_once('=')
        .ok_or_else(|| invalid("approved binding must be NODE_UUID=ENDPOINT_ID"))?;
    let node_id = uuid::Uuid::parse_str(node_id)
        .map_err(|_| invalid("approved binding node ID must be UUIDv7"))?;
    if node_id.get_version_num() != 7 {
        return Err(invalid("approved binding node ID must be UUIDv7"));
    }
    Ok((parse_endpoint(endpoint)?, node_id.as_bytes().to_vec()))
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

#[cfg(target_os = "linux")]
const fn libc_o_nofollow() -> i32 {
    0x20_000
}

#[cfg(target_os = "macos")]
const fn libc_o_nofollow() -> i32 {
    0x100
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
    fn endpoint_parser_requires_canonical_hex() {
        let endpoint = SecretKey::generate().public();
        let encoded = hex::encode(endpoint.as_bytes());
        assert_eq!(parse_endpoint(&encoded).expect("valid endpoint"), endpoint);
        assert!(parse_endpoint(&encoded.to_uppercase()).is_err());
    }

    #[test]
    fn binding_parser_requires_a_uuidv7_node() {
        let node_id = uuid::Uuid::now_v7();
        let endpoint = SecretKey::generate().public();
        let value = format!("{node_id}={}", hex::encode(endpoint.as_bytes()));
        let (parsed_endpoint, parsed_node) = parse_binding(&value).expect("valid binding");
        assert_eq!(parsed_endpoint, endpoint);
        assert_eq!(parsed_node, node_id.as_bytes());
        assert!(
            parse_binding(&format!("not-a-uuid={}", hex::encode(endpoint.as_bytes()))).is_err()
        );
    }

    #[test]
    fn production_relays_require_two_unique_https_urls_and_a_token() {
        let directory =
            std::env::temp_dir().join(format!("ocservia-relay-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("create test directory");
        let token = directory.join("relay.token");
        std::fs::write(&token, "0123456789abcdef0123456789abcdef\n").expect("write token");
        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o600))
            .expect("protect token");

        let mode = build_relay_mode(
            "custom",
            vec![
                "https://relay-a.example.test".to_owned(),
                "https://relay-b.example.test".to_owned(),
            ],
            Some(&token),
        )
        .expect("valid production relay set");
        let RelayMode::Custom(map) = mode else {
            panic!("expected custom relay mode");
        };
        assert_eq!(map.len(), 2);

        assert!(
            build_relay_mode(
                "custom",
                vec!["https://relay-a.example.test".into()],
                Some(&token)
            )
            .is_err()
        );
        std::fs::set_permissions(&token, std::fs::Permissions::from_mode(0o644))
            .expect("make token insecure");
        assert!(
            build_relay_mode(
                "custom",
                vec![
                    "https://relay-a.example.test".into(),
                    "https://relay-b.example.test".into(),
                ],
                Some(&token),
            )
            .is_err()
        );
        assert!(
            build_relay_mode(
                "custom",
                vec![
                    "https://relay-a.example.test".into(),
                    "https://relay-a.example.test".into(),
                ],
                Some(&token),
            )
            .is_err()
        );
        assert!(
            build_relay_mode(
                "custom",
                vec![
                    "http://relay-a.example.test".into(),
                    "https://relay-b.example.test".into(),
                ],
                Some(&token),
            )
            .is_err()
        );
        assert!(
            build_relay_mode(
                "custom",
                vec![
                    "https://relay-a.example.test".into(),
                    "https://relay-b.example.test".into(),
                ],
                None,
            )
            .is_err()
        );
        std::fs::remove_dir_all(directory).expect("remove test directory");
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
}
