#![allow(
    clippy::missing_panics_doc,
    clippy::must_use_candidate,
    clippy::return_self_not_must_use,
    clippy::too_many_arguments
)]

use std::fs;
use std::io;
use std::os::unix::fs::{DirBuilderExt as _, MetadataExt as _, PermissionsExt as _};
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use ed25519_dalek::{Signer as _, SigningKey};
use ocservia_command_authorization::{
    COMMAND_PROTOCOL_VERSION, ControllerCommandKeyring, SessionGrantClaimsV1,
    canonical_session_grant_v1, canonical_v1, claims_from_envelope_v1, semantic_payload_hash_v2,
    verification_key_id,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandAuthorizationProof, CommandDeliveryMode, CommandEnvelope, SemanticPayloadHashVersion,
    SessionGrantV1, command_envelope,
};
use prost_types::Timestamp;
use uuid::Uuid;

pub fn unix_seconds() -> i64 {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("test clock")
            .as_secs(),
    )
    .expect("test time")
}

pub fn deadline_ms(offset_ms: u64) -> u64 {
    u64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("test clock")
            .as_millis(),
    )
    .expect("test time")
        + offset_ms
}

pub fn ts(seconds: i64, nanos: i32) -> Timestamp {
    Timestamp { seconds, nanos }
}

pub fn keyring(key: &SigningKey) -> ControllerCommandKeyring {
    ControllerCommandKeyring::new([key.verifying_key()]).expect("test keyring")
}

pub fn host_architecture() -> &'static str {
    ocservia_contracts::agent_upgrade::runtime_architecture().expect("test host architecture")
}

pub fn foreign_architecture() -> &'static str {
    if host_architecture() == "amd64" {
        "arm64"
    } else {
        "amd64"
    }
}

pub fn write_executable(dir: &Path, name: &str, body: &str) -> PathBuf {
    let path = dir.join(name);
    fs::write(&path, body).expect("write executable fixture");
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755))
        .expect("chmod executable fixture");
    path
}

pub fn sign_session_grant(
    key: &SigningKey,
    node_id: [u8; 16],
    endpoint_id: [u8; 32],
    authorization_revision: u64,
    session_epoch: u64,
    capabilities: Vec<String>,
    issued_at: i64,
    expires_at: i64,
) -> SessionGrantV1 {
    let mut negotiated_capabilities = capabilities;
    negotiated_capabilities.sort();
    negotiated_capabilities.dedup();
    let claims = SessionGrantClaimsV1 {
        version: 1,
        key_id: verification_key_id(&key.verifying_key()),
        protocol_major: 1,
        protocol_minor: 1,
        node_id,
        endpoint_id,
        authorization_revision,
        session_epoch,
        negotiated_capabilities: negotiated_capabilities.clone(),
        issued_at_seconds: issued_at,
        issued_at_nanos: 0,
        expires_at_seconds: expires_at,
        expires_at_nanos: 0,
    };
    let canonical = canonical_session_grant_v1(&claims).expect("canonical session grant");
    let signature = key.sign(&canonical).to_bytes().to_vec();
    SessionGrantV1 {
        version: 1,
        key_id: claims.key_id,
        protocol_major: 1,
        protocol_minor: 1,
        node_id: node_id.to_vec(),
        endpoint_id: endpoint_id.to_vec(),
        authorization_revision,
        session_epoch,
        negotiated_capabilities,
        issued_at: Some(ts(issued_at, 0)),
        expires_at: Some(ts(expires_at, 0)),
        signature,
    }
}

pub fn sign_command_v2(
    key: &SigningKey,
    node_id: [u8; 16],
    issued_at: i64,
    expires_at: i64,
    expected_revision: u64,
    action: &str,
    capability: &str,
    payload: command_envelope::Payload,
) -> CommandEnvelope {
    let mut command = CommandEnvelope {
        protocol_version: COMMAND_PROTOCOL_VERSION.to_owned(),
        message_id: Uuid::now_v7().as_bytes().to_vec(),
        command_id: Uuid::now_v7().as_bytes().to_vec(),
        idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        sequence: 1,
        issued_at: Some(ts(issued_at, 0)),
        expires_at: Some(ts(expires_at, 0)),
        expected_revision,
        actor_id: "controller:test".to_owned(),
        operation_id: Uuid::now_v7().as_bytes().to_vec(),
        action: action.to_owned(),
        required_capability: capability.to_owned(),
        delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
        semantic_payload_hash_version: SemanticPayloadHashVersion::V2.into(),
        payload: Some(payload),
        authorization: Some(CommandAuthorizationProof {
            version: 1,
            key_id: verification_key_id(&key.verifying_key()),
            signature: Vec::new(),
        }),
        ..CommandEnvelope::default()
    };
    sign_command_envelope_v2(key, &mut command);
    command
}

pub fn sign_command_envelope_v2(key: &SigningKey, command: &mut CommandEnvelope) {
    command.semantic_payload_hash_version = SemanticPayloadHashVersion::V2.into();
    command.semantic_payload_sha256 = semantic_payload_hash_v2(command)
        .expect("semantic hash v2")
        .to_vec();
    let key_id = verification_key_id(&key.verifying_key());
    command.authorization = Some(CommandAuthorizationProof {
        version: 1,
        key_id,
        signature: Vec::new(),
    });
    let claims = claims_from_envelope_v1(command).expect("authorization claims");
    let signature = key.sign(&canonical_v1(&claims).expect("canonical authorization"));
    command
        .authorization
        .as_mut()
        .expect("authorization")
        .signature = signature.to_bytes().to_vec();
}

const MAX_UNIX_SOCKET_DIR_BYTES: usize = 80;

/// RAII private `0700` temporary directory outside the repository checkout
/// whose entire ancestor chain up to `/` is real, root-or-self owned, and
/// neither group- nor world-writable (`mode & 0o022 == 0`).
pub struct PrivateTempDir(PathBuf);

impl PrivateTempDir {
    pub fn new(prefix: &str) -> Self {
        let base = resolve_private_temp_base(prefix);
        let dir = create_unique_private_dir(&base, prefix);
        Self(dir)
    }

    pub fn path(&self) -> &Path {
        &self.0
    }

    pub fn join(&self, path: impl AsRef<Path>) -> PathBuf {
        self.0.join(path)
    }

    /// Create every relative component under this directory with mode `0700`
    /// so host `umask` never produces group-writable ancestors.
    pub fn create_subdir_all(&self, relative: impl AsRef<Path>) -> PathBuf {
        let mut current = self.0.clone();
        for component in relative.as_ref().components() {
            match component {
                Component::Normal(part) => {
                    current.push(part);
                    match fs::DirBuilder::new().mode(0o700).create(&current) {
                        Ok(()) => {}
                        Err(err) if err.kind() == io::ErrorKind::AlreadyExists => {}
                        Err(err) => {
                            panic!("create private subdir {}: {err}", current.display());
                        }
                    }
                    fs::set_permissions(&current, fs::Permissions::from_mode(0o700))
                        .expect("chmod private subdir");
                }
                Component::CurDir => {}
                other => panic!("create_subdir_all requires relative path, got {other:?}"),
            }
        }
        current
    }
}

impl AsRef<Path> for PrivateTempDir {
    fn as_ref(&self) -> &Path {
        self.path()
    }
}

impl Drop for PrivateTempDir {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

fn repo_root() -> Option<PathBuf> {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()?
        .parent()?
        .parent()?
        .canonicalize()
        .ok()
}

fn candidate_bases() -> Vec<PathBuf> {
    let mut candidates = Vec::with_capacity(3);
    if let Some(dir) = std::env::var_os("TMPDIR").filter(|v| !v.is_empty()) {
        candidates.push(PathBuf::from(dir));
    }
    if let Some(dir) = std::env::var_os("XDG_RUNTIME_DIR").filter(|v| !v.is_empty()) {
        candidates.push(PathBuf::from(dir));
    }
    // Intentionally use $HOME/.cache rather than $XDG_CACHE_HOME because
    // scripts/env.sh sets XDG_CACHE_HOME inside the repository checkout.
    if let Some(home) = std::env::var_os("HOME").filter(|v| !v.is_empty()) {
        let home_path = PathBuf::from(home);
        let cache = home_path.join(".cache");
        if home_path.is_dir() && !cache.exists() {
            let _ = fs::DirBuilder::new().mode(0o700).create(&cache);
            let _ = fs::set_permissions(&cache, fs::Permissions::from_mode(0o700));
        }
        candidates.push(cache);
    }
    candidates
}

fn select_private_base(
    candidates: &[PathBuf],
    repo: Option<&Path>,
    prefix: &str,
    uid: u32,
) -> Option<PathBuf> {
    for candidate in candidates {
        let Ok(resolved) = fs::canonicalize(candidate) else {
            continue;
        };
        if let Some(root) = repo
            && resolved.starts_with(root)
        {
            continue;
        }
        if !has_safe_ancestry(&resolved, uid) {
            continue;
        }
        if resolved.as_os_str().len() + prefix.len() + 20 > MAX_UNIX_SOCKET_DIR_BYTES {
            continue;
        }
        return Some(resolved);
    }
    None
}

fn resolve_private_temp_base(prefix: &str) -> PathBuf {
    let uid = rustix::process::geteuid().as_raw();
    let repo = repo_root();
    let candidates = candidate_bases();
    select_private_base(&candidates, repo.as_deref(), prefix, uid).unwrap_or_else(|| {
        panic!(
            "no private temporary directory with non-group/world-writable ancestry outside the repository; set TMPDIR to a private 0700 directory"
        )
    })
}

fn has_safe_ancestry(path: &Path, uid: u32) -> bool {
    if !path.is_absolute() {
        return false;
    }
    let mut current = Some(path);
    while let Some(dir) = current {
        let Ok(meta) = fs::symlink_metadata(dir) else {
            return false;
        };
        if !meta.is_dir() || meta.file_type().is_symlink() {
            return false;
        }
        let owner = meta.uid();
        if owner != 0 && owner != uid {
            return false;
        }
        if meta.permissions().mode() & 0o022 != 0 {
            return false;
        }
        current = dir.parent();
    }
    true
}

fn create_unique_private_dir(base: &Path, prefix: &str) -> PathBuf {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    let pid = std::process::id();
    for _ in 0..1024 {
        let seq = NEXT.fetch_add(1, Ordering::Relaxed);
        let dir = base.join(format!("{prefix}-{pid}-{seq}"));
        match fs::DirBuilder::new().mode(0o700).create(&dir) {
            Ok(()) => {
                fs::set_permissions(&dir, fs::Permissions::from_mode(0o700))
                    .expect("chmod private temp dir");
                return dir;
            }
            Err(err) if err.kind() == io::ErrorKind::AlreadyExists => {}
            Err(err) => panic!("create private temp dir {}: {err}", dir.display()),
        }
    }
    panic!("exhausted private temp dir names under {}", base.display());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn private_temp_dir_has_safe_ancestry_outside_repo_and_cleans_up() {
        let uid = rustix::process::geteuid().as_raw();
        let repo = repo_root().expect("repo root");
        let created_path = {
            let dir = PrivateTempDir::new("ts");
            let path = dir.path().to_path_buf();
            assert!(!path.starts_with(&repo), "must be outside repo checkout");
            assert!(has_safe_ancestry(&path, uid), "must have safe ancestry");
            assert_eq!(
                fs::symlink_metadata(&path)
                    .expect("dir meta")
                    .permissions()
                    .mode()
                    & 0o777,
                0o700
            );
            let nested = dir.create_subdir_all("a/b/c");
            assert!(has_safe_ancestry(&nested, uid));
            path
        };
        assert!(!created_path.exists(), "Drop must remove temp dir");
    }

    #[test]
    fn selector_skips_world_writable_and_in_repo_candidates() {
        let uid = rustix::process::geteuid().as_raw();
        let repo = repo_root().expect("repo root");
        let safe = PrivateTempDir::new("sel");
        let candidates = vec![
            PathBuf::from("/tmp"),
            repo.clone(),
            safe.path().to_path_buf(),
        ];
        let picked = select_private_base(&candidates, Some(&repo), "s", uid)
            .expect("falls back to safe candidate");
        assert_eq!(picked, fs::canonicalize(safe.path()).expect("canonical"));
    }
}
