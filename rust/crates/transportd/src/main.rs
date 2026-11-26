use std::env;
use std::io;
#[cfg(test)]
use std::os::unix::fs::PermissionsExt;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
#[cfg(test)]
use std::os::unix::net::UnixStream as StdUnixStream;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use iroh::SecretKey;
use iroh::endpoint::RelayMode;
use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_server::TransportServiceServer;
use ocservia_contracts::relay::{
    load_relay_tls_roots, validate_owner_or_group_readonly_token_file,
};
use ocservia_contracts::uds::{
    authenticate_peer, bind_socket, connect_verified_socket, remove_socket,
};
use ocservia_contracts::unix::O_NOFOLLOW;
use ocservia_transportd::{
    IdentityPolicy, IrohTransportService, TrustAuthority, TrustBudgetConfig, TrustClass,
    build_router_with_trust_tls_roots,
};
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
    ocservia_observability::init_with_default_filter(
        "ocservia-transportd",
        env!("CARGO_PKG_VERSION"),
        "info,iroh=warn",
    )?;
    let config = parse_args()?;
    if let Some(stats_file) = config.stats_file.clone() {
        ocservia_observability::spawn_runtime_stats_writer(stats_file)?;
    }
    let key = load_key(&config.key_file)?;
    let (listener, socket_identity) = bind_socket(&config.socket)?;
    let keyring = load_controller_keyring(&config)?;
    let relay_tls_roots = load_relay_tls_roots(config.relay_ca_file.as_deref())?;
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
            ocservia_observability::shutdown_signal().await;
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
    let mut trust_budgets = TrustBudgetConfig::default();
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
                let value = parse_usize(
                    &required_value(&mut args, "--known-agent-handshake-rate")?,
                    "known Agent handshake rate",
                )?;
                trust_budgets
                    .update_limit(TrustClass::KnownAgentHandshake, |limit| {
                        limit.attempts_per_minute = value;
                    })
                    .map_err(invalid)?;
            }
            "--known-agent-handshake-concurrency" => {
                let value = parse_usize(
                    &required_value(&mut args, "--known-agent-handshake-concurrency")?,
                    "known Agent handshake concurrency",
                )?;
                trust_budgets
                    .update_limit(TrustClass::KnownAgentHandshake, |limit| {
                        limit.concurrency = value;
                    })
                    .map_err(invalid)?;
            }
            "--agent-authorization-rate" => {
                let value = parse_usize(
                    &required_value(&mut args, "--agent-authorization-rate")?,
                    "Agent authorization rate",
                )?;
                trust_budgets
                    .update_limit(TrustClass::AgentAuthorization, |limit| {
                        limit.attempts_per_minute = value;
                    })
                    .map_err(invalid)?;
            }
            "--agent-authorization-concurrency" => {
                let value = parse_usize(
                    &required_value(&mut args, "--agent-authorization-concurrency")?,
                    "Agent authorization concurrency",
                )?;
                trust_budgets
                    .update_limit(TrustClass::AgentAuthorization, |limit| {
                        limit.concurrency = value;
                    })
                    .map_err(invalid)?;
            }
            "--agent-registration-recheck-rate" => {
                let value = parse_usize(
                    &required_value(&mut args, "--agent-registration-recheck-rate")?,
                    "Agent registration recheck rate",
                )?;
                trust_budgets
                    .update_limit(TrustClass::AgentRegistrationRecheck, |limit| {
                        limit.attempts_per_minute = value;
                    })
                    .map_err(invalid)?;
            }
            "--agent-registration-recheck-concurrency" => {
                let value = parse_usize(
                    &required_value(&mut args, "--agent-registration-recheck-concurrency")?,
                    "Agent registration recheck concurrency",
                )?;
                trust_budgets
                    .update_limit(TrustClass::AgentRegistrationRecheck, |limit| {
                        limit.concurrency = value;
                    })
                    .map_err(invalid)?;
            }
            "--unknown-enrollment-preauth-rate" => {
                let value = parse_usize(
                    &required_value(&mut args, "--unknown-enrollment-preauth-rate")?,
                    "unknown enrollment pre-auth rate",
                )?;
                trust_budgets
                    .update_limit(TrustClass::UnknownEnrollmentPreAuth, |limit| {
                        limit.attempts_per_minute = value;
                    })
                    .map_err(invalid)?;
            }
            "--unknown-enrollment-preauth-concurrency" => {
                let value = parse_usize(
                    &required_value(&mut args, "--unknown-enrollment-preauth-concurrency")?,
                    "unknown enrollment pre-auth concurrency",
                )?;
                trust_budgets
                    .update_limit(TrustClass::UnknownEnrollmentPreAuth, |limit| {
                        limit.concurrency = value;
                    })
                    .map_err(invalid)?;
            }
            "--enrollment-completion-rate" => {
                let value = parse_usize(
                    &required_value(&mut args, "--enrollment-completion-rate")?,
                    "enrollment completion rate",
                )?;
                trust_budgets
                    .update_limit(TrustClass::EnrollmentCompletion, |limit| {
                        limit.attempts_per_minute = value;
                    })
                    .map_err(invalid)?;
            }
            "--enrollment-completion-concurrency" => {
                let value = parse_usize(
                    &required_value(&mut args, "--enrollment-completion-concurrency")?,
                    "enrollment completion concurrency",
                )?;
                trust_budgets
                    .update_limit(TrustClass::EnrollmentCompletion, |limit| {
                        limit.concurrency = value;
                    })
                    .map_err(invalid)?;
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
    let owner = rustix::process::geteuid().as_raw();
    let group = rustix::process::getegid().as_raw();
    ocservia_contracts::relay::build_relay_mode(mode, raw_url, token_file, |file| {
        validate_owner_or_group_readonly_token_file(file, owner, group)
    })
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
        .custom_flags(O_NOFOLLOW)
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

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use ocservia_test_support::PrivateTempDir;
    use std::io::Write;

    fn socket_test_directory() -> PrivateTempDir {
        PrivateTempDir::new("s")
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
            assert!(validate_owner_or_group_readonly_token_file(&file, uid, gid).is_ok());
        }
        assert!(validate_key_file(&file, uid).is_err());
        assert!(
            validate_owner_or_group_readonly_token_file(&file, uid.wrapping_add(1), gid).is_err()
        );
        assert!(
            validate_owner_or_group_readonly_token_file(&file, uid, gid.wrapping_add(1)).is_err()
        );
        for mode in [0o640, 0o444, 0o644, 0o460] {
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(mode)).unwrap();
            assert!(validate_owner_or_group_readonly_token_file(&file, uid, gid).is_err());
        }
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o440)).unwrap();
        std::fs::hard_link(&path, directory.join("alias")).unwrap();
        assert!(validate_owner_or_group_readonly_token_file(&file, uid, gid).is_err());
        std::fs::remove_dir_all(directory).unwrap();
    }

    #[tokio::test]
    async fn second_instance_cannot_replace_a_live_socket() {
        let directory = socket_test_directory();
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
    }

    #[tokio::test]
    async fn shutdown_does_not_unlink_a_replacement_socket() {
        let directory = socket_test_directory();
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
    }

    #[tokio::test]
    async fn transport_server_rejects_a_foreign_peer_uid() {
        let directory = socket_test_directory();
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
    }

    #[tokio::test]
    async fn trust_client_rejects_a_foreign_owned_socket() {
        let directory = socket_test_directory();
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
    }

    #[test]
    fn nofollow_flag_matches_platform_and_rejects_symlinks() {
        assert_eq!(
            O_NOFOLLOW,
            i32::try_from(rustix::fs::OFlags::NOFOLLOW.bits()).expect("NOFOLLOW fits in i32")
        );
        let directory = socket_test_directory();
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
            build_relay_mode(
                "custom",
                Some("https://relay.example.test"),
                Some(&link_token),
            )
            .is_err(),
            "symlinked relay token must be rejected"
        );
    }
}
