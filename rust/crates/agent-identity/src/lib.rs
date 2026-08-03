//! Durable Agent endpoint identity and controller identity pinning.

#![forbid(unsafe_code)]

use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt};
use std::path::Path;

use iroh::{EndpointId, SecretKey};
use zeroize::Zeroizing;

const KEY_FILE: &str = "endpoint.key";
const CONTROLLER_FILE: &str = "controller.endpoint";

/// A long-lived Agent endpoint key paired with its immutable controller pin.
#[derive(Clone)]
pub struct Identity {
    key: SecretKey,
    controller: EndpointId,
}

impl std::fmt::Debug for Identity {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Identity")
            .field("endpoint_id", &self.key.public())
            .field("controller", &self.controller)
            .finish()
    }
}

impl Identity {
    /// Creates the endpoint key once and refuses any later controller substitution.
    ///
    /// # Errors
    ///
    /// Returns an error for insecure paths, invalid identity data, or a changed pin.
    pub fn provision(directory: &Path, controller: EndpointId) -> Result<Self, io::Error> {
        ensure_directory(directory)?;
        let key_path = directory.join(KEY_FILE);
        let pin_path = directory.join(CONTROLLER_FILE);
        let key = match secure_open(&key_path) {
            Ok(mut file) => read_key(&mut file)?,
            Err(error) if error.kind() == io::ErrorKind::NotFound => create_key(&key_path)?,
            Err(error) => return Err(error),
        };
        match secure_open(&pin_path) {
            Ok(mut file) => {
                let pinned = read_endpoint(&mut file)?;
                if pinned != controller {
                    return Err(io::Error::new(
                        io::ErrorKind::PermissionDenied,
                        "controller endpoint substitution refused",
                    ));
                }
            }
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                create_secret_file(&pin_path, hex::encode(controller.as_bytes()).as_bytes())?;
            }
            Err(error) => return Err(error),
        }
        File::open(directory)?.sync_all()?;
        Ok(Self { key, controller })
    }

    /// Returns the public Agent `EndpointID`.
    #[must_use]
    pub fn endpoint_id(&self) -> EndpointId {
        self.key.public()
    }

    /// Returns the pinned controller `EndpointID`.
    #[must_use]
    pub const fn controller_endpoint_id(&self) -> EndpointId {
        self.controller
    }

    /// Borrows the secret key without exposing it through formatting.
    #[must_use]
    pub const fn secret_key(&self) -> &SecretKey {
        &self.key
    }
}

fn ensure_directory(path: &Path) -> Result<(), io::Error> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) => {
            if !metadata.is_dir()
                || metadata.file_type().is_symlink()
                || metadata.uid() != rustix::process::geteuid().as_raw()
                || metadata.mode() & 0o077 != 0
            {
                return Err(io::Error::new(
                    io::ErrorKind::PermissionDenied,
                    "identity directory must be owner-only",
                ));
            }
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            std::fs::DirBuilder::new().mode(0o700).create(path)?;
        }
        Err(error) => return Err(error),
    }
    Ok(())
}

fn create_key(path: &Path) -> Result<SecretKey, io::Error> {
    let key = SecretKey::generate();
    let bytes = Zeroizing::new(key.to_bytes());
    create_secret_file(path, bytes.as_ref())?;
    Ok(key)
}

fn create_secret_file(path: &Path, contents: &[u8]) -> Result<(), io::Error> {
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)?;
    file.write_all(contents)?;
    file.sync_all()?;
    Ok(())
}

fn secure_open(path: &Path) -> Result<File, io::Error> {
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(no_follow())
        .open(path)?;
    let metadata = file.metadata()?;
    if !metadata.is_file()
        || metadata.uid() != rustix::process::geteuid().as_raw()
        || metadata.mode() & 0o077 != 0
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "identity file must be owner-only",
        ));
    }
    Ok(file)
}

fn read_key(file: &mut File) -> Result<SecretKey, io::Error> {
    let mut bytes = Zeroizing::new(Vec::with_capacity(33));
    file.take(33).read_to_end(&mut bytes)?;
    let raw: [u8; 32] = bytes.as_slice().try_into().map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            "endpoint key must contain 32 bytes",
        )
    })?;
    Ok(SecretKey::from_bytes(&raw))
}

fn read_endpoint(file: &mut File) -> Result<EndpointId, io::Error> {
    let mut text = String::new();
    file.take(65).read_to_string(&mut text)?;
    if text.len() != 64 || text.bytes().any(|byte| byte.is_ascii_uppercase()) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "controller endpoint pin is invalid",
        ));
    }
    let bytes: [u8; 32] = hex::decode(text)
        .map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                "controller endpoint pin is invalid",
            )
        })?
        .try_into()
        .map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                "controller endpoint pin is invalid",
            )
        })?;
    EndpointId::from_bytes(&bytes).map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            "controller endpoint pin is invalid",
        )
    })
}

#[cfg(target_os = "linux")]
const fn no_follow() -> i32 {
    0x20_000
}
#[cfg(target_os = "macos")]
const fn no_follow() -> i32 {
    0x100
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn test_dir() -> PathBuf {
        std::env::temp_dir().join(format!("ocservia-agent-identity-{}", std::process::id()))
    }

    #[test]
    fn endpoint_key_is_stable_and_controller_substitution_is_rejected() {
        let directory = test_dir();
        let _ = std::fs::remove_dir_all(&directory);
        let controller = SecretKey::generate().public();
        let first = Identity::provision(&directory, controller).expect("provision identity");
        let second = Identity::provision(&directory, controller).expect("reload identity");
        assert_eq!(first.endpoint_id(), second.endpoint_id());
        let error = Identity::provision(&directory, SecretKey::generate().public())
            .expect_err("substitution rejected");
        assert_eq!(error.kind(), io::ErrorKind::PermissionDenied);
        std::fs::remove_dir_all(directory).expect("remove identity fixture");
    }
}
