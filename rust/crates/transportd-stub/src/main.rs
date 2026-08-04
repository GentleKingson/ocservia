use std::env;
use std::io;
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
    if let Some(path) = config.stats_file.clone() {
        tokio::spawn(write_stats(service.clone(), path));
    }
    let (health_reporter, health_service) = tonic_health::server::health_reporter();
    health_reporter
        .set_serving::<TransportServiceServer<StubService>>()
        .await;

    let result = Server::builder()
        .add_service(health_service)
        .add_service(TransportServiceServer::new(service))
        .serve_with_incoming_shutdown(UnixListenerStream::new(listener), shutdown())
        .await;
    remove_socket(&config.socket)?;
    result?;
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

async fn write_stats(service: StubService, path: PathBuf) {
    let mut ticker = tokio::time::interval(Duration::from_secs(1));
    loop {
        ticker.tick().await;
        let stats = service.stats().await;
        let document = stats_json(stats);
        if let Err(error) = tokio::fs::write(&path, document).await {
            eprintln!("failed to write simulator stats: {error}");
            return;
        }
    }
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
}
