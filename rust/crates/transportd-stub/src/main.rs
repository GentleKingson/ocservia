use std::env;
use std::io;
use std::io::Write;
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::time::Duration;

use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_server::TransportServiceServer;
use ocservia_transportd_stub::{StubService, StubStats};
use tokio::net::UnixListener;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let config = parse_args()?;
    let listener = bind_socket(&config.socket)?;
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
    let server = Server::builder()
        .add_service(health_service)
        .add_service(TransportServiceServer::new(service))
        .serve_with_incoming_shutdown(UnixListenerStream::new(listener), shutdown());
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
    let socket_result = remove_socket(&config.socket);
    result?;
    socket_result?;
    Ok(())
}

struct Config {
    socket: PathBuf,
    queue_capacity: usize,
    capacity_telemetry: bool,
    stats_file: Option<PathBuf>,
}

fn parse_args() -> Result<Config, io::Error> {
    let mut config = Config {
        socket: PathBuf::from("/run/ocserv-platform/transportd.sock"),
        queue_capacity: 256,
        capacity_telemetry: false,
        stats_file: None,
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
        || config
            .stats_file
            .as_ref()
            .is_some_and(|path| !path.is_absolute())
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "socket and stats paths must be absolute and queue capacity must be 1..4096",
        ));
    }
    Ok(config)
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

fn bind_socket(path: &Path) -> Result<UnixListener, io::Error> {
    let parent = path
        .parent()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "socket path has no parent"))?;
    if !std::fs::metadata(parent)?.is_dir() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "socket parent is not a directory",
        ));
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

async fn shutdown() {
    let _signal = tokio::signal::ctrl_c().await;
}

#[cfg(test)]
mod tests {
    use super::*;

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
    async fn bind_socket_preserves_existing_parent_permissions() {
        let parent = PathBuf::from("/tmp").join(format!("ocservia-stub-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&parent).expect("create test directory");
        std::fs::set_permissions(&parent, std::fs::Permissions::from_mode(0o1777))
            .expect("set test directory permissions");
        let socket = parent.join("transportd.sock");

        let listener = bind_socket(&socket).expect("bind socket");
        let mode = std::fs::metadata(&parent)
            .expect("inspect test directory")
            .permissions()
            .mode()
            & 0o7777;

        assert_eq!(mode, 0o1777);
        drop(listener);
        remove_socket(&socket).expect("remove socket");
        std::fs::remove_dir(parent).expect("remove test directory");
    }

    #[test]
    fn stats_are_published_atomically_and_replace_existing_snapshot() {
        let parent = PathBuf::from("/tmp").join(format!("ocservia-stats-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&parent).expect("create stats test directory");
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
        std::fs::remove_dir_all(parent).expect("remove stats test directory");
    }

    #[test]
    fn stats_publish_errors_do_not_leave_temporary_file() {
        let parent = PathBuf::from("/tmp").join(format!("ocservia-stats-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&parent).expect("create stats test directory");
        let path = parent.join("missing").join("stats.json");

        assert!(publish_stats(&path, br#"{}"#).is_err());
        assert!(!parent.join("missing").join("stats.json.tmp").exists());
        std::fs::remove_dir_all(parent).expect("remove stats test directory");
    }

    #[test]
    fn concurrent_stats_reads_never_observe_partial_json() {
        let parent = PathBuf::from("/tmp").join(format!("ocservia-stats-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&parent).expect("create stats test directory");
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
        std::fs::remove_dir_all(parent).expect("remove stats test directory");
    }
}
