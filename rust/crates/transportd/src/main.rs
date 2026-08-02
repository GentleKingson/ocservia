use std::collections::HashSet;
use std::env;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

use iroh::endpoint::RelayMode;
use iroh::{EndpointId, SecretKey};
use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_server::TransportServiceServer;
use ocservia_transportd::{IdentityPolicy, IrohTransportService, build_router};
use tokio::net::UnixListener;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;
use zeroize::Zeroizing;

#[derive(Debug)]
struct Config {
    socket: PathBuf,
    key_file: PathBuf,
    relay_mode: RelayMode,
    approved: HashSet<EndpointId>,
    revoked: HashSet<EndpointId>,
    event_capacity: usize,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,iroh=warn".into()),
        )
        .init();
    let config = parse_args()?;
    let key = load_key(&config.key_file)?;
    let listener = bind_socket(&config.socket)?;
    let service = IrohTransportService::new(config.event_capacity);
    let router = build_router(
        key,
        config.relay_mode,
        IdentityPolicy::new(config.approved, config.revoked),
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
    let result = Server::builder()
        .add_service(health_service)
        .add_service(TransportServiceServer::new(service.clone()))
        .serve_with_incoming_shutdown(UnixListenerStream::new(listener), shutdown_signal())
        .await;
    health_reporter
        .set_not_serving::<TransportServiceServer<IrohTransportService>>()
        .await;
    let router_result = ocservia_transportd::shutdown(&service, router).await;
    let socket_result = remove_socket(&config.socket);
    result?;
    router_result?;
    socket_result?;
    Ok(())
}

fn parse_args() -> Result<Config, io::Error> {
    let mut socket = PathBuf::from("/run/ocserv-platform/transportd.sock");
    let mut key_file = None;
    let mut relay_mode = RelayMode::Default;
    let mut approved = HashSet::new();
    let mut revoked = HashSet::new();
    let mut event_capacity = 256_usize;
    let mut args = env::args().skip(1);
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--socket" => socket = PathBuf::from(required_value(&mut args, "--socket")?),
            "--key-file" => {
                key_file = Some(PathBuf::from(required_value(&mut args, "--key-file")?));
            }
            "--relay-mode" => {
                relay_mode = match required_value(&mut args, "--relay-mode")?.as_str() {
                    "default" => RelayMode::Default,
                    "disabled" => RelayMode::Disabled,
                    _ => return Err(invalid("relay mode must be default or disabled")),
                };
            }
            "--approved-endpoint" => {
                approved.insert(parse_endpoint(&required_value(
                    &mut args,
                    "--approved-endpoint",
                )?)?);
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
            _ => return Err(invalid(&format!("unknown argument: {argument}"))),
        }
    }
    let key_file = key_file.ok_or_else(|| invalid("--key-file is required"))?;
    if !socket.is_absolute() || !key_file.is_absolute() {
        return Err(invalid("socket and key file paths must be absolute"));
    }
    if event_capacity == 0 || event_capacity > 4096 {
        return Err(invalid("event capacity must be 1..4096"));
    }
    Ok(Config {
        socket,
        key_file,
        relay_mode,
        approved,
        revoked,
        event_capacity,
    })
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

fn load_key(path: &Path) -> Result<SecretKey, io::Error> {
    let metadata = std::fs::symlink_metadata(path)?;
    if !metadata.file_type().is_file() || metadata.file_type().is_symlink() {
        return Err(invalid("key path must be a regular file, not a symlink"));
    }
    if metadata.mode() & 0o077 != 0 {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "key file must be owned by the process user with mode 0600 or stricter",
        ));
    }
    let bytes = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc_o_nofollow())
        .open(path)
        .and_then(|mut file| {
            let mut bytes = Vec::with_capacity(65);
            std::io::Read::read_to_end(&mut file, &mut bytes)?;
            Ok(bytes)
        })?;
    let bytes = Zeroizing::new(bytes);
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

#[cfg(target_os = "linux")]
const fn libc_o_nofollow() -> i32 {
    0x20_000
}

#[cfg(target_os = "macos")]
const fn libc_o_nofollow() -> i32 {
    0x100
}

fn bind_socket(path: &Path) -> Result<UnixListener, io::Error> {
    let parent = path
        .parent()
        .ok_or_else(|| invalid("socket path has no parent"))?;
    if !std::fs::metadata(parent)?.is_dir() {
        return Err(invalid("socket parent is not a directory"));
    }
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_socket() => std::fs::remove_file(path)?,
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
    Ok(listener)
}

fn remove_socket(path: &Path) -> Result<(), io::Error> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_socket() => std::fs::remove_file(path),
        Ok(_) => Err(io::Error::other("socket path changed during shutdown")),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.to_owned())
}

async fn shutdown_signal() {
    if tokio::signal::ctrl_c().await.is_err() {
        tracing::error!("failed to install shutdown signal handler");
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

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
    fn endpoint_parser_requires_canonical_hex() {
        let endpoint = SecretKey::generate().public();
        let encoded = hex::encode(endpoint.as_bytes());
        assert_eq!(parse_endpoint(&encoded).expect("valid endpoint"), endpoint);
        assert!(parse_endpoint(&encoded.to_uppercase()).is_err());
    }
}
