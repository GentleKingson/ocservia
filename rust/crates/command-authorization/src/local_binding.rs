//! Root-provisioned local authority selector shared by Agent and privd.

use std::fs::File;
use std::io::{self, Read as _};
use std::os::unix::fs::MetadataExt as _;
use std::path::{Component, Path, PathBuf};

use ed25519_dalek::VerifyingKey;
use rustix::fs::{Mode, OFlags};
use uuid::Uuid;

/// The sole publication point for an explicitly committed local rebind.
pub const ACTIVE_BINDING: &str = "/etc/ocservia-agent/active-binding";

/// All authority in one root-owned atomic record. Paths are derived from the
/// fresh Controller-side node UUID, never supplied by a remote command.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LocalBinding {
    pub node_id: Uuid,
    pub controller: [u8; 32],
    pub endpoint: [u8; 32],
    pub command_key: VerifyingKey,
    /// An unresolved retired effect quarantines mutations across namespaces.
    pub mutations_blocked: bool,
}

impl LocalBinding {
    /// Reads the fixed root-owned selector. Absence preserves initial-install
    /// configuration; malformed or inaccessible state never falls back.
    ///
    /// # Errors
    /// Refuses unsafe paths, invalid authority or incomplete records.
    pub fn load_active() -> io::Result<Option<Self>> {
        match Self::load(Path::new(ACTIVE_BINDING), 0) {
            Ok(binding) => Ok(Some(binding)),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error),
        }
    }

    fn load(path: &Path, owner: u32) -> io::Result<Self> {
        let mut components = path.components();
        if components.next() != Some(Component::RootDir) {
            return Err(invalid());
        }
        let names = components
            .map(|part| match part {
                Component::Normal(name) => Ok(name),
                _ => Err(invalid()),
            })
            .collect::<io::Result<Vec<_>>>()?;
        let (name, parents) = names.split_last().ok_or_else(invalid)?;
        let mut directory = File::from(rustix::fs::open(
            "/",
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
            Mode::empty(),
        )?);
        for parent in parents {
            super::validate_directory(&directory, owner)?;
            directory = File::from(rustix::fs::openat(
                &directory,
                *parent,
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )?);
        }
        super::validate_directory(&directory, owner)?;
        let file = File::from(rustix::fs::openat(
            &directory,
            *name,
            OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )?);
        let metadata = file.metadata()?;
        if !metadata.is_file()
            || metadata.uid() != owner
            || metadata.nlink() != 1
            || !matches!(metadata.mode() & 0o777, 0o440 | 0o640)
        {
            return Err(invalid());
        }
        let mut text = String::new();
        file.take(1025).read_to_string(&mut text)?;
        Self::decode(&text)
    }

    /// Parses the canonical local record, not a wire-protocol message.
    ///
    /// # Errors
    /// Rejects unknown versions, malformed identities and noncanonical records.
    pub fn decode(text: &str) -> io::Result<Self> {
        let fields: Vec<_> = text.lines().collect();
        if fields.len() != 6 || fields[0] != "ocservia-binding-v1" || text.len() > 1024 {
            return Err(invalid());
        }
        let node_id = Uuid::parse_str(fields[1]).map_err(|_| invalid())?;
        if node_id.get_version_num() != 7 || node_id.to_string() != fields[1] {
            return Err(invalid());
        }
        let binding = Self {
            node_id,
            controller: decode_hex(fields[2])?,
            endpoint: decode_hex(fields[3])?,
            command_key: VerifyingKey::from_bytes(&decode_hex(fields[4])?)
                .map_err(|_| invalid())?,
            mutations_blocked: match fields[5] {
                "clear" => false,
                "blocked" => true,
                _ => return Err(invalid()),
            },
        };
        if binding.encode() != text {
            return Err(invalid());
        }
        Ok(binding)
    }

    #[must_use]
    pub fn encode(&self) -> String {
        format!(
            "ocservia-binding-v1\n{}\n{}\n{}\n{}\n{}\n",
            self.node_id,
            hex::encode(self.controller),
            hex::encode(self.endpoint),
            hex::encode(self.command_key.as_bytes()),
            if self.mutations_blocked {
                "blocked"
            } else {
                "clear"
            }
        )
    }

    #[must_use]
    pub fn agent_directory(&self) -> PathBuf {
        Path::new("/var/lib/ocservia-agent/bindings").join(self.node_id.to_string())
    }

    #[must_use]
    pub fn effect_directory(&self) -> PathBuf {
        Path::new("/var/lib/ocservia-privd/bindings").join(self.node_id.to_string())
    }

    #[must_use]
    pub fn upgrade_directory(&self) -> PathBuf {
        Path::new("/var/lib/ocservia-upgrade/bindings")
            .join(self.node_id.to_string())
            .join("operations")
    }
}

fn decode_hex(text: &str) -> io::Result<[u8; 32]> {
    if text.len() != 64
        || !text
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(invalid());
    }
    hex::decode(text)
        .map_err(|_| invalid())?
        .try_into()
        .map_err(|_| invalid())
}

fn invalid() -> io::Error {
    io::Error::new(
        io::ErrorKind::InvalidData,
        "invalid or unsafe local Controller binding",
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn selector_refuses_links_and_writable_or_untrusted_files() {
        use std::os::unix::fs::PermissionsExt as _;
        let root = std::env::current_dir()
            .unwrap()
            .join(format!(".binding-test-{}", std::process::id()));
        std::fs::create_dir(&root).unwrap();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        let path = root.join("binding");
        let record = format!(
            "ocservia-binding-v1\n018f0c2e-7b1a-7c3d-8e9f-0123456789ab\n{}\n{}\n{}\nclear\n",
            hex::encode([1; 32]),
            hex::encode([2; 32]),
            hex::encode(
                ed25519_dalek::SigningKey::from_bytes(&[3; 32])
                    .verifying_key()
                    .as_bytes()
            )
        );
        std::fs::write(&path, record).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o640)).unwrap();
        let owner = rustix::process::geteuid().as_raw();
        assert!(LocalBinding::load(&path, owner).is_ok());
        std::os::unix::fs::symlink(&path, root.join("symlink")).unwrap();
        assert!(LocalBinding::load(&root.join("symlink"), owner).is_err());
        std::fs::hard_link(&path, root.join("hardlink")).unwrap();
        assert!(LocalBinding::load(&path, owner).is_err());
        std::fs::remove_file(root.join("hardlink")).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o660)).unwrap();
        assert!(LocalBinding::load(&path, owner).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn binding_is_canonical_and_isolates_all_state_paths() {
        let old = LocalBinding {
            node_id: Uuid::parse_str("018f0c2e-7b1a-7c3d-8e9f-0123456789ab").unwrap(),
            controller: [1; 32],
            endpoint: [2; 32],
            command_key: ed25519_dalek::SigningKey::from_bytes(&[3; 32]).verifying_key(),
            mutations_blocked: true,
        };
        assert_eq!(LocalBinding::decode(&old.encode()).unwrap(), old);
        let mut next = old.clone();
        next.node_id = Uuid::parse_str("018f0c2e-7b1a-7c3d-8e9f-0123456789ac").unwrap();
        assert_ne!(old.agent_directory(), next.agent_directory());
        assert_ne!(old.effect_directory(), next.effect_directory());
        assert_ne!(old.upgrade_directory(), next.upgrade_directory());
        for malformed in [
            old.encode().to_uppercase(),
            old.encode().replace("blocked", ""),
            old.encode().replace("binding-v1", "binding-v2"),
            old.encode().replace("7c3d", "4c3d"),
            format!("{}extra\n", old.encode()),
        ] {
            assert!(LocalBinding::decode(&malformed).is_err());
        }
    }
}
