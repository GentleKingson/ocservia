//! Verified local Unix domain socket binding, peer authentication, and cleanup.

use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::os::unix::net::UnixStream as StdUnixStream;
use std::path::{Component, Path};

use tokio::net::{UnixListener, UnixStream};

/// Device and inode identity of a verified socket inode.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SocketIdentity {
    /// `st_dev` of the bound socket inode.
    pub device: u64,
    /// `st_ino` of the bound socket inode.
    pub inode: u64,
}

/// Validates socket ancestry, safely clears any stale dead socket, binds `path`
/// with mode `0o660`, and returns the listener alongside its inode identity.
///
/// # Errors
///
/// Refuses non-normalized paths, attacker-writable parent directories,
/// non-socket path replacement, and live sockets owned by another process.
#[allow(clippy::similar_names)]
pub fn bind_socket(path: &Path) -> Result<(UnixListener, SocketIdentity), io::Error> {
    let expected_uid = rustix::process::geteuid().as_raw();
    let expected_gid = rustix::process::getegid().as_raw();
    validate_ancestry(path, expected_uid)?;
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_socket() => {
            validate_socket(path, expected_uid, expected_gid)?;
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
    let identity = validate_socket(path, expected_uid, expected_gid)?;
    Ok((listener, identity))
}

/// Verifies `path` is absolute, component-normalized, and every ancestor
/// directory is owned by root or `expected_owner` without group/world write bits.
///
/// # Errors
///
/// Returns `InvalidInput` or `PermissionDenied` on unsafe path ancestry, or
/// forwards underlying filesystem metadata errors.
pub fn validate_ancestry(path: &Path, expected_owner: u32) -> Result<(), io::Error> {
    if !path.is_absolute()
        || path
            .components()
            .any(|component| !matches!(component, Component::RootDir | Component::Normal(_)))
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "Unix socket path must be absolute and normalized",
        ));
    }
    let mut current = path
        .parent()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "socket path has no parent"))?;
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

/// Validates ancestry and socket ownership/permissions (`0o660`), returning the
/// bound socket's device and inode.
///
/// # Errors
///
/// Returns `InvalidInput`, `PermissionDenied`, or metadata I/O errors.
#[allow(clippy::similar_names)]
pub fn validate_socket(
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

/// Connects to `path` after verifying safe socket ownership/permissions and
/// confirming both peer `UID` and unchanged socket inode after connect.
///
/// # Errors
///
/// Returns `PermissionDenied` or I/O errors when socket validation or peer
/// authentication fails.
#[allow(clippy::similar_names)]
pub async fn connect_verified_socket(
    path: &Path,
    expected_uid: u32,
    expected_gid: u32,
) -> Result<UnixStream, io::Error> {
    let identity = validate_socket(path, expected_uid, expected_gid)?;
    let stream = UnixStream::connect(path).await?;
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

/// Verifies the connected Unix socket peer's effective `UID` matches `expected_uid`.
///
/// # Errors
///
/// Returns `PermissionDenied` on UID mismatch or forwards `peer_cred()` I/O errors.
pub fn authenticate_peer(stream: &UnixStream, expected_uid: u32) -> Result<(), io::Error> {
    let credentials = stream.peer_cred()?;
    if credentials.uid() != expected_uid {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "Unix socket client UID is not authorized",
        ));
    }
    Ok(())
}

/// Unlinks `path` only if its device and inode still match `expected`.
///
/// # Errors
///
/// Returns `io::Error::other` if the path was replaced with another inode, or
/// forwards filesystem errors other than `NotFound`.
pub fn remove_socket(path: &Path, expected: SocketIdentity) -> Result<(), io::Error> {
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
