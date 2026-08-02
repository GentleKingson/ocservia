use std::env;
use std::io;
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::{Path, PathBuf};

use ocservia_contracts::generated::ocserv::platform::transport::v1::transport_service_server::TransportServiceServer;
use ocservia_transportd_stub::StubService;
use tokio::net::UnixListener;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let (socket, queue_capacity) = parse_args()?;
    let listener = bind_socket(&socket)?;
    let service = StubService::new(queue_capacity);
    let (health_reporter, health_service) = tonic_health::server::health_reporter();
    health_reporter
        .set_serving::<TransportServiceServer<StubService>>()
        .await;

    let result = Server::builder()
        .add_service(health_service)
        .add_service(TransportServiceServer::new(service))
        .serve_with_incoming_shutdown(UnixListenerStream::new(listener), shutdown())
        .await;
    remove_socket(&socket)?;
    result?;
    Ok(())
}

fn parse_args() -> Result<(PathBuf, usize), io::Error> {
    let mut socket = PathBuf::from("/run/ocserv-platform/transportd.sock");
    let mut queue_capacity = 256_usize;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--socket" => {
                socket = PathBuf::from(args.next().ok_or_else(|| {
                    io::Error::new(io::ErrorKind::InvalidInput, "--socket requires a path")
                })?);
            }
            "--queue-capacity" => {
                queue_capacity = args
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
            _ => {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("unknown argument: {arg}"),
                ));
            }
        }
    }
    if !socket.is_absolute() || queue_capacity == 0 || queue_capacity > 4096 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "socket must be absolute and queue capacity must be 1..4096",
        ));
    }
    Ok((socket, queue_capacity))
}

fn bind_socket(path: &Path) -> Result<UnixListener, io::Error> {
    let parent = path
        .parent()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "socket path has no parent"))?;
    std::fs::create_dir_all(parent)?;
    std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o750))?;
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
