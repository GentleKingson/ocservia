use std::env;
use std::io;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::Duration;

use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_server::TransportServiceServer;
use ocservia_contracts::uds::{authenticate_peer, bind_socket, remove_socket};
use ocservia_transportd_stub::{StubService, StubStats};
use tokio_stream::StreamExt;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let config = parse_args()?;
    let (listener, socket_identity) = bind_socket(&config.socket)?;
    let service = if config.capacity_telemetry {
        StubService::new_capacity(config.queue_capacity)
    } else {
        StubService::new(config.queue_capacity)
    };
    let (health_reporter, health_service) = tonic_health::server::health_reporter();
    health_reporter
        .set_serving::<TransportServiceServer<StubService>>()
        .await;

    let stats_service = service.clone();
    let control_plane_uid = config.control_plane_uid;
    let incoming =
        UnixListenerStream::new(listener).filter_map(move |connection| match connection {
            Ok(stream) => authenticate_peer(&stream, control_plane_uid)
                .ok()
                .map(|()| Ok(stream)),
            Err(error) => Some(Err(error)),
        });
    let server = Server::builder()
        .add_service(health_service)
        .add_service(TransportServiceServer::new(service))
        .serve_with_incoming_shutdown(incoming, ocservia_observability::shutdown_signal());
    tokio::pin!(server);
    let result: Result<(), Box<dyn std::error::Error>> = if let Some(path) = config.stats_file {
        let mut writer = tokio::spawn(write_stats(stats_service, path));
        tokio::select! {
            result = &mut server => {
                writer.abort();
                result.map_err(Into::into)
            }
            result = &mut writer => {
                let error = match result {
                    Ok(Err(error)) => error,
                    Ok(Ok(())) => io::Error::other("simulator stats writer stopped unexpectedly"),
                    Err(error) => io::Error::other(format!("simulator stats writer task failed: {error}")),
                };
                Err(error.into())
            }
        }
    } else {
        server.await.map_err(Into::into)
    };
    let socket_result = remove_socket(&config.socket, socket_identity);
    result?;
    socket_result?;
    Ok(())
}

struct Config {
    socket: PathBuf,
    queue_capacity: usize,
    capacity_telemetry: bool,
    stats_file: Option<PathBuf>,
    control_plane_uid: u32,
    control_plane_gid: u32,
}

fn parse_args() -> Result<Config, io::Error> {
    let mut config = Config {
        socket: PathBuf::from("/run/ocserv-platform/transportd.sock"),
        queue_capacity: 256,
        capacity_telemetry: false,
        stats_file: None,
        control_plane_uid: u32::MAX,
        control_plane_gid: u32::MAX,
    };
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--socket" => {
                config.socket = PathBuf::from(args.next().ok_or_else(|| {
                    io::Error::new(io::ErrorKind::InvalidInput, "--socket requires a path")
                })?);
            }
            "--queue-capacity" => {
                config.queue_capacity = args
                    .next()
                    .ok_or_else(|| {
                        io::Error::new(
                            io::ErrorKind::InvalidInput,
                            "--queue-capacity requires a value",
                        )
                    })?
                    .parse()
                    .map_err(|_| {
                        io::Error::new(io::ErrorKind::InvalidInput, "invalid --queue-capacity")
                    })?;
            }
            "--capacity-telemetry" => config.capacity_telemetry = true,
            "--stats-file" => {
                config.stats_file = Some(PathBuf::from(args.next().ok_or_else(|| {
                    io::Error::new(io::ErrorKind::InvalidInput, "--stats-file requires a path")
                })?));
            }
            "--control-plane-uid" => {
                config.control_plane_uid = parse_u32(
                    &args.next().ok_or_else(|| {
                        io::Error::new(
                            io::ErrorKind::InvalidInput,
                            "--control-plane-uid requires a value",
                        )
                    })?,
                    "control-plane UID",
                )?;
            }
            "--control-plane-gid" => {
                config.control_plane_gid = parse_u32(
                    &args.next().ok_or_else(|| {
                        io::Error::new(
                            io::ErrorKind::InvalidInput,
                            "--control-plane-gid requires a value",
                        )
                    })?,
                    "control-plane GID",
                )?;
            }
            _ => {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("unknown argument: {arg}"),
                ));
            }
        }
    }
    if !config.socket.is_absolute()
        || config.queue_capacity == 0
        || config.queue_capacity > 4096
        || config.control_plane_uid == u32::MAX
        || config.control_plane_gid == u32::MAX
        || config
            .stats_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "socket and stats paths must be absolute, peer identity is required, and queue capacity must be 1..4096",
        ));
    }
    Ok(config)
}

fn parse_u32(value: &str, name: &str) -> io::Result<u32> {
    value.parse::<u32>().map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("{name} must be an unsigned 32-bit integer"),
        )
    })
}

async fn write_stats(service: StubService, path: PathBuf) -> io::Result<()> {
    let mut ticker = tokio::time::interval(Duration::from_secs(1));
    loop {
        ticker.tick().await;
        let stats = service.stats().await;
        let document = stats_json(stats);
        publish_stats(&path, &document)?;
    }
}

fn publish_stats(path: &Path, document: &[u8]) -> io::Result<()> {
    let file_name = path.file_name().ok_or_else(|| {
        io::Error::new(io::ErrorKind::InvalidInput, "stats path has no file name")
    })?;
    let mut temporary_name = file_name.to_os_string();
    temporary_name.push(".tmp");
    let temporary = path.with_file_name(temporary_name);
    match std::fs::remove_file(&temporary) {
        Ok(()) => {}
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let result = (|| {
        let mut file = std::fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)?;
        file.write_all(document)?;
        file.flush()?;
        drop(file);
        std::fs::rename(&temporary, path)
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&temporary);
    }
    result
}

fn stats_json(stats: StubStats) -> Vec<u8> {
    serde_json::to_vec(&serde_json::json!({
        "connected_nodes": stats.connected_nodes,
        "retained_events": stats.retained_events,
        "subscribers": stats.subscribers,
        "accepted_commands": stats.accepted_commands,
        "active_tasks": stats.active_tasks,
        "task_capacity": stats.task_capacity,
    }))
    .expect("simulator stats contain only integers")
}

#[cfg(test)]
mod tests {
    use super::*;
    use ocservia_test_support::PrivateTempDir;
    use std::os::unix::fs::PermissionsExt;

    fn socket_test_directory() -> PrivateTempDir {
        PrivateTempDir::new("s")
    }

    fn empty_stats() -> StubStats {
        StubStats {
            connected_nodes: 0,
            retained_events: 0,
            subscribers: 0,
            accepted_commands: 0,
            active_tasks: 0,
            task_capacity: 0,
        }
    }

    #[tokio::test]
    async fn bind_socket_requires_private_ancestry() {
        let parent = socket_test_directory();
        let socket = parent.join("s");

        let (listener, identity) = bind_socket(&socket).expect("bind socket");
        let mode = std::fs::metadata(parent.path())
            .expect("inspect test directory")
            .permissions()
            .mode()
            & 0o7777;

        assert_eq!(mode, 0o700);
        drop(listener);
        remove_socket(&socket, identity).expect("remove socket");
    }

    #[tokio::test]
    async fn bind_socket_rejects_client_writable_ancestry() {
        let parent = socket_test_directory();
        std::fs::set_permissions(parent.path(), std::fs::Permissions::from_mode(0o770))
            .expect("set test directory permissions");
        let socket = parent.join("s");
        assert_eq!(
            bind_socket(&socket)
                .expect_err("writable ancestry rejected")
                .kind(),
            io::ErrorKind::PermissionDenied
        );
    }

    #[test]
    fn stats_are_published_atomically_and_replace_existing_snapshot() {
        let parent = PrivateTempDir::new("stats");
        let path = parent.join("stats.json");
        std::fs::write(&path, br#"{"old":true}"#).expect("write old snapshot");
        let document = stats_json(StubStats {
            connected_nodes: 1,
            retained_events: 2,
            subscribers: 3,
            accepted_commands: 4,
            active_tasks: 5,
            task_capacity: 8,
        });

        publish_stats(&path, &document).expect("publish stats");

        let published = std::fs::read(&path).expect("read published stats");
        let value: serde_json::Value = serde_json::from_slice(&published).expect("valid JSON");
        assert_eq!(value["connected_nodes"], 1);
        assert_eq!(value["retained_events"], 2);
        assert_eq!(value["subscribers"], 3);
        assert_eq!(value["accepted_commands"], 4);
        assert_eq!(value["active_tasks"], 5);
        assert_eq!(value["task_capacity"], 8);
        assert!(!parent.join("stats.json.tmp").exists());
    }

    #[test]
    fn stats_publish_errors_do_not_leave_temporary_file() {
        let parent = PrivateTempDir::new("stats");
        let path = parent.join("missing").join("stats.json");

        assert!(publish_stats(&path, br"{}").is_err());
        assert!(!parent.join("missing").join("stats.json.tmp").exists());
    }

    #[test]
    fn concurrent_stats_reads_never_observe_partial_json() {
        let parent = PrivateTempDir::new("stats");
        let path = parent.join("stats.json");
        publish_stats(&path, &stats_json(empty_stats())).expect("publish initial stats");
        let writer_path = path.clone();
        let writer = std::thread::spawn(move || {
            for value in 1..=100 {
                publish_stats(
                    &writer_path,
                    &stats_json(StubStats {
                        connected_nodes: value,
                        ..empty_stats()
                    }),
                )
                .expect("publish replacement stats");
            }
        });

        while !writer.is_finished() {
            let document = std::fs::read(&path).expect("read stats during publication");
            let _: serde_json::Value =
                serde_json::from_slice(&document).expect("complete JSON during publication");
        }
        writer.join().expect("stats writer thread");
        assert!(!parent.join("stats.json.tmp").exists());
    }
}
