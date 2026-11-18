//! Version-aware Ocserv adapter with fixed resources, executables, and arguments.

#![forbid(unsafe_code)]

mod certificates;
mod complete_config;
mod configuration;
mod effect_store;
mod user_file;
use effect_store::{
    EffectStore, PrepareEffect, effect_now, reject_symlink, validate_effect_identity,
};
use user_file::authoritative_user_owner;
pub use user_file::parse_user_file;

use std::collections::{HashMap, HashSet};
use std::ffi::OsString;
use std::fs::{File, OpenOptions as StdOpenOptions};
use std::io::{self, Write};
use std::net::IpAddr;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Component, Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
#[cfg(test)]
use std::sync::atomic::{AtomicU8, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_agent_protocol::{
    ArtifactData, CertificateArtifactResult, CertificateCsrResult, CertificateRevokeResult,
    ConfigApplyResult, ConfigFingerprint, ConfigPlanResult, DesiredEffectObservation,
    DesiredEffectState, ErrorKind, GroupList, IpBan, IpBanList, MAX_FRAME_BYTES,
    MAX_MANAGED_RESOURCES, MutationResult, ObservedGroup, ObservedUser, OcservVersion, PrivdError,
    ServiceStatus, Session, SessionList, UserList,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    SealedSecretPurpose, SealedSecretV1, SealedSecretVersion,
};
use rand::Rng;
use rusqlite::{Connection, OpenFlags, OptionalExtension, TransactionBehavior, params};
use sha2::{Digest, Sha256};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::process::Command;
use tokio::sync::Mutex;
use uuid::Uuid;
use zeroize::Zeroizing;

const MAX_CONFIG_BYTES: usize = 1024 * 1024;
const MAX_CONFIG_PLAN_BYTES: usize = 256 * 1024;
const MAX_USER_FILE_BYTES: usize = 16 * 1024 * 1024;
const DEFAULT_OUTPUT_BYTES: usize = 256 * 1024;
const EFFECT_STORE_KEY_BYTES: usize = 32;
const MAX_EFFECT_RECORDS: i64 = 65_536;
const ARTIFACT_CHUNK_BYTES: usize = 256 * 1024;
const MAX_ARTIFACT_BYTES: usize = 64 * 1024 * 1024;
const USER_FILE_MODE: u32 = 0o600;

#[derive(Debug)]
struct StagingFile {
    path: PathBuf,
    armed: bool,
}

#[derive(Debug)]
struct ProcessGroupGuard {
    raw_pid: Option<i32>,
}

impl ProcessGroupGuard {
    fn new(child: &tokio::process::Child) -> Self {
        Self {
            raw_pid: child.id().and_then(|pid| i32::try_from(pid).ok()),
        }
    }

    fn disarm(&mut self) {
        self.raw_pid = None;
    }
}

impl Drop for ProcessGroupGuard {
    fn drop(&mut self) {
        let Some(pid) = self.raw_pid.and_then(rustix::process::Pid::from_raw) else {
            return;
        };
        let _ = rustix::process::kill_process_group(pid, rustix::process::Signal::KILL);
    }
}

impl StagingFile {
    fn new(path: PathBuf) -> Self {
        Self { path, armed: true }
    }

    fn path(&self) -> &Path {
        &self.path
    }

    fn disarm(&mut self) {
        self.armed = false;
    }

    async fn remove(&mut self) -> Result<(), AdapterError> {
        match tokio::fs::remove_file(&self.path).await {
            Ok(()) => {
                self.disarm();
                Ok(())
            }
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                self.disarm();
                Ok(())
            }
            Err(error) => Err(AdapterError::Io(error)),
        }
    }
}

impl Drop for StagingFile {
    fn drop(&mut self) {
        if self.armed {
            let _ = std::fs::remove_file(&self.path);
        }
    }
}

fn valid_dns_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 253
        && !value.starts_with('.')
        && !value.ends_with('.')
        && value.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && !label.starts_with('-')
                && !label.ends_with('-')
                && label
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
}

fn redacted_candidate_diff(candidate: &str) -> Result<String, AdapterError> {
    let mut output = String::from("- <current configuration redacted>\n");
    for line in candidate.lines() {
        if line.starts_with('#') {
            continue;
        }
        let (name, value) = line.split_once(" = ").ok_or(AdapterError::InvalidRequest)?;
        output.push_str("+ ");
        output.push_str(name);
        output.push_str(" = ");
        output.push_str(value);
        output.push('\n');
    }
    if output.len() > MAX_CONFIG_PLAN_BYTES {
        return Err(AdapterError::OutputLimit);
    }
    Ok(output)
}

/// Root-controlled fixed local resources used by the adapter.
#[derive(Clone, Debug)]
pub struct FixedResources {
    systemctl: PathBuf,
    ocserv: PathBuf,
    occtl: PathBuf,
    config: PathBuf,
    boot_id: PathBuf,
    ocpasswd: PathBuf,
    user_file: PathBuf,
    openssl: PathBuf,
    user_secret_key: PathBuf,
    user_secret_key_id: String,
    p12_secret_key: PathBuf,
    p12_secret_key_id: String,
    effect_store: PathBuf,
    effect_store_key: PathBuf,
    mutations_blocked: bool,
    certificate_key_dir: PathBuf,
    config_tls_root: PathBuf,
}

impl Default for FixedResources {
    fn default() -> Self {
        Self {
            systemctl: PathBuf::from("/usr/bin/systemctl"),
            ocserv: PathBuf::from("/usr/sbin/ocserv"),
            occtl: PathBuf::from("/usr/bin/occtl"),
            config: PathBuf::from("/etc/ocserv/ocserv.conf"),
            boot_id: PathBuf::from("/proc/sys/kernel/random/boot_id"),
            ocpasswd: PathBuf::from("/usr/bin/ocpasswd"),
            user_file: PathBuf::from("/etc/ocserv/ocpasswd"),
            openssl: PathBuf::from("/usr/bin/openssl"),
            user_secret_key: PathBuf::from("/etc/ocservia/user-password-seal-private.pem"),
            user_secret_key_id: String::from("user-default"),
            p12_secret_key: PathBuf::from("/etc/ocservia/p12-password-seal-private.pem"),
            p12_secret_key_id: String::from("p12-default"),
            effect_store: PathBuf::from("/var/lib/ocservia-privd/desired-effects.sqlite3"),
            effect_store_key: PathBuf::from("/var/lib/ocservia-privd/desired-effects.key"),
            mutations_blocked: false,
            certificate_key_dir: PathBuf::from("/var/lib/ocservia-privd/certificates"),
            config_tls_root: PathBuf::from("/etc/ocservia-agent/config-tls"),
        }
    }
}

impl FixedResources {
    /// Quarantines privileged writes while a retired binding has unresolved effects.
    #[must_use]
    pub fn with_mutations_blocked(mut self, blocked: bool) -> Self {
        self.mutations_blocked = blocked;
        self
    }

    /// Constructs resources from trusted process-startup configuration.
    ///
    /// These values are never populated from an Agent RPC.
    ///
    /// # Errors
    ///
    /// Rejects non-absolute paths.
    pub fn new(
        systemctl: PathBuf,
        ocserv: PathBuf,
        occtl: PathBuf,
        config: PathBuf,
        boot_id: PathBuf,
    ) -> Result<Self, AdapterError> {
        for path in [&systemctl, &ocserv, &occtl, &config, &boot_id] {
            if !path.is_absolute() {
                return Err(AdapterError::InvalidResource);
            }
        }
        Ok(Self {
            systemctl,
            ocserv,
            occtl,
            config,
            boot_id,
            ..Self::default()
        })
    }

    /// Overrides only fixed root-owned user resources at process startup.
    ///
    /// # Errors
    ///
    /// Rejects non-absolute paths and invalid configured key identifiers.
    pub fn with_user_resources(
        mut self,
        ocpasswd: PathBuf,
        user_file: PathBuf,
        openssl: PathBuf,
        secret_key: PathBuf,
        secret_key_id: String,
    ) -> Result<Self, AdapterError> {
        for path in [&ocpasswd, &user_file, &openssl, &secret_key] {
            if !path.is_absolute() {
                return Err(AdapterError::InvalidResource);
            }
        }
        if !valid_key_id(&secret_key_id) {
            return Err(AdapterError::InvalidResource);
        }
        self.ocpasswd = ocpasswd;
        self.user_file = user_file;
        self.openssl = openssl;
        self.user_secret_key = secret_key;
        self.user_secret_key_id = secret_key_id;
        Ok(self)
    }

    /// Overrides the fixed P12-password unsealing key at process startup.
    ///
    /// # Errors
    ///
    /// Rejects non-absolute paths and invalid configured key identifiers.
    pub fn with_p12_resources(
        mut self,
        secret_key: PathBuf,
        secret_key_id: String,
    ) -> Result<Self, AdapterError> {
        if !secret_key.is_absolute() || !valid_key_id(&secret_key_id) {
            return Err(AdapterError::InvalidResource);
        }
        self.p12_secret_key = secret_key;
        self.p12_secret_key_id = secret_key_id;
        Ok(self)
    }

    /// Configures the two purpose-separated production unsealing keys.
    ///
    /// # Errors
    ///
    /// Rejects reused paths or identifiers and invalid key configuration.
    pub fn with_password_sealing_keys(
        mut self,
        user_key: PathBuf,
        user_key_id: String,
        p12_key: PathBuf,
        p12_key_id: String,
    ) -> Result<Self, AdapterError> {
        if !user_key.is_absolute()
            || !p12_key.is_absolute()
            || user_key == p12_key
            || !valid_key_id(&user_key_id)
            || !valid_key_id(&p12_key_id)
            || user_key_id == p12_key_id
        {
            return Err(AdapterError::InvalidResource);
        }
        self.user_secret_key = user_key;
        self.user_secret_key_id = user_key_id;
        self.p12_secret_key = p12_key;
        self.p12_secret_key_id = p12_key_id;
        Ok(self)
    }

    /// Overrides the fixed root-owned reconciliation store for isolated deployments or tests.
    ///
    /// # Errors
    ///
    /// Rejects non-absolute paths or colocated database and key files.
    pub fn with_effect_store(
        mut self,
        effect_store: PathBuf,
        effect_store_key: PathBuf,
    ) -> Result<Self, AdapterError> {
        if !effect_store.is_absolute()
            || !effect_store_key.is_absolute()
            || effect_store == effect_store_key
        {
            return Err(AdapterError::InvalidResource);
        }
        self.effect_store = effect_store;
        self.effect_store_key = effect_store_key;
        Ok(self)
    }

    /// Overrides the fixed private-key directory for isolated deployments or tests.
    ///
    /// # Errors
    ///
    /// Returns [`AdapterError::InvalidResource`] when the directory is not absolute.
    pub fn with_certificate_key_dir(mut self, directory: PathBuf) -> Result<Self, AdapterError> {
        if !directory.is_absolute() {
            return Err(AdapterError::InvalidResource);
        }
        self.certificate_key_dir = directory;
        Ok(self)
    }
}

/// Non-secret controller command identity bound into desired-effect evidence.
#[derive(Clone, Copy, Debug)]
pub struct EffectIdentity<'a> {
    /// Stable controller command UUID bytes.
    pub command_id: &'a [u8],
    /// Stable idempotency UUID bytes.
    pub idempotency_key: &'a [u8],
    /// Canonical semantic payload SHA-256.
    pub semantic_payload_sha256: &'a [u8],
    /// Command expiry after which evidence may be safely garbage-collected.
    pub expires_at_unix_seconds: i64,
}

/// Controller-verified identity for one privileged command effect. Every field
/// is derived by privd from the signed command rather than supplied as an
/// independent Agent assertion.
#[derive(Clone, Copy, Debug)]
pub struct AuthorizedEffectIdentity<'a> {
    pub node_id: &'a [u8],
    pub command_id: &'a [u8],
    pub operation_id: &'a [u8],
    pub idempotency_key: &'a [u8],
    pub semantic_payload_sha256: &'a [u8],
    pub action: &'a str,
    pub authorization_revision: u64,
    pub effect_kind: &'a str,
    pub resource_key: &'a str,
    pub effect_revision: u64,
    pub expires_at_unix_seconds: i64,
    pub retry_if_absent: bool,
}

/// Exact, independently verified Controller grant identity used by the
/// root-owned artifact lifecycle store.
#[derive(Clone, Copy, Debug)]
pub struct ArtifactLeaseIdentity<'a> {
    pub artifact_id: &'a [u8],
    pub certificate_id: &'a [u8],
    pub certificate_version: u64,
    pub operation_id: &'a [u8],
    pub authorized_subject: &'a str,
    pub max_bytes: u64,
    pub grant_id: &'a [u8],
    pub expires_at_unix_seconds: i64,
}

/// Durable command-level idempotency decision returned before a root effect.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AuthorizedEffectDecision {
    Execute,
    Replay(Vec<u8>),
    Pending,
}

/// Adapter execution limits.
#[derive(Clone, Copy, Debug)]
pub struct Limits {
    /// Maximum child execution time.
    pub timeout: Duration,
    /// Maximum retained bytes per stdout/stderr stream.
    pub output_bytes: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            timeout: Duration::from_secs(5),
            output_bytes: DEFAULT_OUTPUT_BYTES,
        }
    }
}

/// Fixed-resource Ocserv adapter.
#[derive(Clone, Debug)]
pub struct Adapter {
    resources: FixedResources,
    limits: Limits,
    user_file_lock: Arc<Mutex<()>>,
    config_plan_lock: Arc<Mutex<()>>,
    certificate_artifact_lock: Arc<Mutex<()>>,
    #[cfg(test)]
    config_apply_fault: Arc<AtomicU8>,
}

impl Adapter {
    /// Creates an adapter with fixed trusted resources.
    #[must_use]
    pub fn new(resources: FixedResources, limits: Limits) -> Self {
        Self {
            resources,
            limits,
            user_file_lock: Arc::new(Mutex::new(())),
            config_plan_lock: Arc::new(Mutex::new(())),
            certificate_artifact_lock: Arc::new(Mutex::new(())),
            #[cfg(test)]
            config_apply_fault: Arc::new(AtomicU8::new(0)),
        }
    }

    /// Durably reserves one Controller-authorized privileged effect before it
    /// can reach a root-owned adapter operation.
    ///
    /// # Errors
    ///
    /// Rejects malformed, expired, conflicting, or unauthenticated stored
    /// identities and unavailable effect evidence.
    pub fn prepare_authorized_effect(
        &self,
        identity: AuthorizedEffectIdentity<'_>,
    ) -> Result<AuthorizedEffectDecision, AdapterError> {
        if self.resources.mutations_blocked {
            return Err(AdapterError::Unavailable);
        }
        EffectStore::open_for_mutation(&self.resources)?.prepare_authorized(identity)
    }

    /// Persists the bounded successful response for exact replay without a
    /// second privileged side effect.
    ///
    /// # Errors
    ///
    /// Rejects missing/conflicting reservations and oversized responses.
    pub fn complete_authorized_effect(
        &self,
        identity: AuthorizedEffectIdentity<'_>,
        response: &[u8],
    ) -> Result<(), AdapterError> {
        EffectStore::open_existing(&self.resources)?.complete_authorized(identity, response)
    }

    /// Reads the exact authenticated terminal response for reconciliation.
    /// It never creates, advances, or retries a privileged effect.
    ///
    /// # Errors
    ///
    /// Rejects identity conflicts, corrupted records, and unavailable storage.
    pub fn replay_authorized_effect(
        &self,
        identity: AuthorizedEffectIdentity<'_>,
    ) -> Result<Option<Vec<u8>>, AdapterError> {
        EffectStore::open_existing(&self.resources)?.replay_authorized(identity)
    }

    #[cfg(test)]
    fn inject_config_apply_fault(&self, point: u8) {
        self.config_apply_fault.store(point, Ordering::SeqCst);
    }

    #[cfg(test)]
    fn take_config_apply_fault(&self, point: u8) -> bool {
        self.config_apply_fault
            .compare_exchange(point, 0, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok()
    }

    /// Checks the host's read-only startup snapshot before enrollment.
    /// This does not initialize stores, clean staging, or mutate ocserv.
    ///
    /// # Errors
    /// Returns the same bounded resource/parser errors as ordinary snapshots.
    pub async fn host_preflight(&self) -> Result<(), AdapterError> {
        self.user_list().await?;
        self.group_list().await?;
        self.ocserv_version().await?;
        self.config_fingerprint().await?;
        self.service_status().await?;
        self.session_list().await?;
        self.ip_ban_list().await?;
        Ok(())
    }

    /// Returns the fixed `ocserv.service` state.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn service_status(&self) -> Result<ServiceStatus, AdapterError> {
        let output = self
            .execute(
                &self.resources.systemctl,
                &[
                    "show",
                    "ocserv.service",
                    "--property=LoadState",
                    "--property=ActiveState",
                    "--property=SubState",
                    "--no-pager",
                ],
            )
            .await?;
        parse_service_status(&output.stdout)
    }

    /// Returns the installed Ocserv version.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn ocserv_version(&self) -> Result<OcservVersion, AdapterError> {
        let output = self.execute(&self.resources.ocserv, &["--version"]).await?;
        // Packaged Ocserv writes its banner to stderr; retain stdout compatibility.
        let banner = if output.stderr.is_empty() {
            &output.stdout
        } else {
            &output.stderr
        };
        parse_version(banner)
    }

    /// Returns current sessions parsed into a stable DTO.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn session_list(&self) -> Result<SessionList, AdapterError> {
        let output = self
            .execute(&self.resources.occtl, &["--json", "show", "users"])
            .await?;
        parse_sessions(&output.stdout)
    }

    /// Returns current IP bans parsed into a stable DTO.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for child or parser failures.
    pub async fn ip_ban_list(&self) -> Result<IpBanList, AdapterError> {
        let output = self
            .execute(&self.resources.occtl, &["--json", "show", "ip", "bans"])
            .await?;
        parse_ip_bans(&output.stdout)
    }

    /// Disconnects exactly one numeric session from the current host boot.
    ///
    /// # Errors
    ///
    /// Rejects a malformed or stale target and propagates bounded child failures.
    pub async fn session_disconnect(
        &self,
        session_id: &str,
        boot_id: &str,
    ) -> Result<MutationResult, AdapterError> {
        self.ensure_boot_id(boot_id).await?;
        let session_id = numeric_session_id(session_id)?;
        self.execute(&self.resources.occtl, &["disconnect", "id", session_id])
            .await?;
        Ok(MutationResult { applied: true })
    }

    /// Terminates exactly one numeric session and invalidates its reconnect cookie.
    ///
    /// # Errors
    ///
    /// Rejects a malformed or stale target, or an Ocserv whose `occtl` has no
    /// `terminate` command, before any child runs; propagates bounded child failures.
    pub async fn session_terminate(
        &self,
        session_id: &str,
        boot_id: &str,
    ) -> Result<MutationResult, AdapterError> {
        self.ensure_boot_id(boot_id).await?;
        let session_id = numeric_session_id(session_id)?;
        if !ocservia_agent_protocol::supports_session_terminate(
            &self.ocserv_version().await?.version,
        ) {
            return Err(AdapterError::InvalidRequest);
        }
        self.execute(&self.resources.occtl, &["terminate", "id", session_id])
            .await?;
        Ok(MutationResult { applied: true })
    }

    /// Removes exactly one canonical address from the Ocserv ban list.
    ///
    /// # Errors
    ///
    /// Rejects a noncanonical address and propagates bounded child failures.
    pub async fn ip_ban_remove(&self, ip: &str) -> Result<MutationResult, AdapterError> {
        let canonical = canonical_ip(ip)?;
        if canonical != ip {
            return Err(AdapterError::InvalidRequest);
        }
        self.execute(&self.resources.occtl, &["unban", "ip", &canonical])
            .await?;
        Ok(MutationResult { applied: true })
    }

    /// Reloads only the fixed `ocserv.service` systemd unit.
    ///
    /// # Errors
    ///
    /// Propagates bounded failures from the fixed systemctl invocation.
    pub async fn service_reload(&self) -> Result<MutationResult, AdapterError> {
        self.execute(
            &self.resources.systemctl,
            &["reload", "ocserv.service", "--no-ask-password"],
        )
        .await?;
        Ok(MutationResult { applied: true })
    }

    async fn ensure_boot_id(&self, expected: &str) -> Result<(), AdapterError> {
        if Uuid::parse_str(expected).is_err() {
            return Err(AdapterError::InvalidRequest);
        }
        let actual = tokio::fs::read_to_string(&self.resources.boot_id)
            .await
            .map_err(AdapterError::Io)?;
        if actual.trim() != expected {
            return Err(AdapterError::StaleBoot);
        }
        Ok(())
    }

    async fn execute(&self, program: &Path, args: &[&str]) -> Result<ChildOutput, AdapterError> {
        self.execute_with_input(program, args, &[]).await
    }

    async fn execute_with_input(
        &self,
        program: &Path,
        args: &[&str],
        input: &[u8],
    ) -> Result<ChildOutput, AdapterError> {
        let mut command = Command::new(program);
        command
            .args(args)
            .process_group(0)
            .stdin(if input.is_empty() {
                Stdio::null()
            } else {
                Stdio::piped()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        let mut child = command.spawn().map_err(AdapterError::Io)?;
        let mut process_group = ProcessGroupGuard::new(&child);
        let stdin = child.stdin.take();
        let stdout = child.stdout.take().ok_or(AdapterError::Unavailable)?;
        let stderr = child.stderr.take().ok_or(AdapterError::Unavailable)?;
        // One deadline covers stdin, process exit and inherited output pipes.
        // Borrowed futures are dropped on cancellation instead of detached readers.
        let completed = tokio::time::timeout(self.limits.timeout, async {
            tokio::try_join!(
                async {
                    if let Some(mut stdin) = stdin {
                        stdin.write_all(input).await.map_err(AdapterError::Io)?;
                        stdin.shutdown().await.map_err(AdapterError::Io)?;
                    }
                    Ok::<(), AdapterError>(())
                },
                async { child.wait().await.map_err(AdapterError::Io) },
                read_bounded(stdout, self.limits.output_bytes),
                read_bounded(stderr, self.limits.output_bytes),
            )
        })
        .await;
        let ((), status, stdout, stderr) = if let Ok(result) = completed {
            result?
        } else {
            // child.id() is already None if the parent exited before its pipes.
            drop(process_group);
            let _ = child.wait().await;
            return Err(AdapterError::DeadlineExceeded);
        };
        process_group.disarm();
        if stdout.exceeded || stderr.exceeded {
            return Err(AdapterError::OutputLimit);
        }
        if !status.success() {
            return Err(AdapterError::CommandFailed {
                code: status.code(),
            });
        }
        Ok(ChildOutput {
            stdout: stdout.bytes,
            stderr: stderr.bytes,
        })
    }
}

fn validate_name(value: &str) -> Result<(), AdapterError> {
    if value.is_empty()
        || value.len() > 64
        || !value.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_alphanumeric() || (index > 0 && matches!(byte, b'_' | b'.' | b'-'))
        })
    {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(())
}

fn valid_key_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'-'))
}

#[derive(Clone, Copy)]
struct FileIdentity {
    mode: u32,
    uid: u32,
    gid: u32,
}

async fn file_identity(path: &Path) -> Result<FileIdentity, AdapterError> {
    match tokio::fs::metadata(path).await {
        Ok(metadata) => Ok(FileIdentity {
            mode: metadata.permissions().mode() & 0o777,
            uid: metadata.uid(),
            gid: metadata.gid(),
        }),
        Err(error) => Err(AdapterError::Io(error)),
    }
}

fn lock_config_file(config: &Path) -> Result<std::fs::File, AdapterError> {
    let parent = config.parent().ok_or(AdapterError::InvalidResource)?;
    let path = parent.join(".ocservia-config.lock");
    reject_symlink(&path)?;
    let mut options = StdOpenOptions::new();
    options.read(true).write(true).create(true).mode(0o600);
    let file = options.open(path).map_err(AdapterError::Io)?;
    rustix::fs::flock(&file, rustix::fs::FlockOperation::LockExclusive)
        .map_err(|error| AdapterError::Io(error.into()))?;
    Ok(file)
}

async fn atomic_replace(staging: &Path, path: &Path) -> Result<(), AdapterError> {
    let file = tokio::fs::OpenOptions::new()
        .read(true)
        .open(staging)
        .await
        .map_err(AdapterError::Io)?;
    file.sync_all().await.map_err(AdapterError::Io)?;
    tokio::fs::rename(staging, path)
        .await
        .map_err(AdapterError::Io)?;
    let parent = path.parent().ok_or(AdapterError::InvalidResource)?;
    sync_directory(parent).await
}

async fn write_new_synced(
    path: &Path,
    bytes: &[u8],
    identity: FileIdentity,
) -> Result<(), AdapterError> {
    let mut options = tokio::fs::OpenOptions::new();
    options.write(true).create_new(true).mode(identity.mode);
    let mut file = options.open(path).await.map_err(AdapterError::Io)?;
    rustix::fs::fchown(
        &file,
        Some(rustix::fs::Uid::from_raw(identity.uid)),
        Some(rustix::fs::Gid::from_raw(identity.gid)),
    )
    .map_err(|error| AdapterError::Io(error.into()))?;
    file.set_permissions(std::fs::Permissions::from_mode(identity.mode))
        .await
        .map_err(AdapterError::Io)?;
    file.write_all(bytes).await.map_err(AdapterError::Io)?;
    file.sync_all().await.map_err(AdapterError::Io)
}

async fn sync_directory(parent: &Path) -> Result<(), AdapterError> {
    let directory = tokio::fs::File::open(parent)
        .await
        .map_err(AdapterError::Io)?;
    directory.sync_all().await.map_err(AdapterError::Io)
}

#[derive(Debug)]
struct ChildOutput {
    stdout: Vec<u8>,
    stderr: Vec<u8>,
}

#[derive(Debug)]
struct BoundedOutput {
    bytes: Vec<u8>,
    exceeded: bool,
}

async fn read_bounded(
    mut reader: impl AsyncRead + Unpin,
    limit: usize,
) -> Result<BoundedOutput, AdapterError> {
    let mut bytes = Vec::with_capacity(limit.min(4096));
    let mut buffer = [0_u8; 4096];
    let mut exceeded = false;
    loop {
        let count = reader.read(&mut buffer).await.map_err(AdapterError::Io)?;
        if count == 0 {
            break;
        }
        let remaining = limit.saturating_sub(bytes.len());
        if count > remaining {
            bytes.extend_from_slice(&buffer[..remaining]);
            exceeded = true;
        } else {
            bytes.extend_from_slice(&buffer[..count]);
        }
    }
    Ok(BoundedOutput { bytes, exceeded })
}

/// Stable adapter failures.
#[derive(Debug)]
pub enum AdapterError {
    /// Trusted resources must be absolute paths.
    InvalidResource,
    /// A fixed resource failed a metadata check; values contain no file contents.
    ResourceMetadata {
        resource: &'static str,
        reason: &'static str,
        actual: u64,
        expected: u64,
    },
    /// A fixed resource could not be opened without following links.
    ResourceIo {
        resource: &'static str,
        reason: &'static str,
        source: io::Error,
    },
    /// A password record failed parsing; only physical positions and counts are retained.
    UserFileFormat {
        reason: &'static str,
        line: usize,
        fields: usize,
        first_line: Option<usize>,
    },
    /// An operation argument was not in its canonical typed form.
    InvalidRequest,
    /// A session identifier belongs to a prior host boot.
    StaleBoot,
    /// Fixed local resource was unavailable.
    Unavailable,
    /// Child exceeded its deadline.
    DeadlineExceeded,
    /// stdout or stderr exceeded the configured bound.
    OutputLimit,
    /// A desired mutation would exceed a bounded authoritative-state capacity.
    CapacityExceeded,
    /// Fixed child returned nonzero.
    CommandFailed { code: Option<i32> },
    /// Output did not match a supported version fixture.
    MalformedOutput,
    /// Local I/O failure.
    Io(io::Error),
}

impl std::fmt::Display for AdapterError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::InvalidResource => write!(formatter, "fixed resource path invalid"),
            Self::ResourceMetadata {
                resource,
                reason,
                actual,
                expected,
            } => {
                if *reason == "mode" || *reason == "writable_mode_bits" {
                    write!(
                        formatter,
                        "resource={resource} reason={reason} actual={actual:04o} expected={expected:04o}"
                    )
                } else {
                    write!(
                        formatter,
                        "resource={resource} reason={reason} actual={actual} expected={expected}"
                    )
                }
            }
            Self::ResourceIo {
                resource,
                reason,
                source,
            } => write!(formatter, "resource={resource} reason={reason}: {source}"),
            Self::UserFileFormat {
                reason,
                line,
                fields,
                first_line,
            } => {
                write!(
                    formatter,
                    "resource=ocpasswd reason={reason} line={line} fields={fields} expected_fields=3"
                )?;
                if let Some(first) = first_line {
                    write!(formatter, " first_line={first}")?;
                }
                Ok(())
            }
            Self::InvalidRequest => write!(formatter, "fixed operation argument invalid"),
            Self::StaleBoot => write!(formatter, "session boot identity is stale"),
            Self::Unavailable => write!(formatter, "fixed local resource unavailable"),
            Self::DeadlineExceeded => write!(formatter, "fixed read operation timed out"),
            Self::OutputLimit => write!(formatter, "fixed read operation output limit exceeded"),
            Self::CapacityExceeded => write!(formatter, "authoritative state capacity exceeded"),
            Self::CommandFailed { code } => {
                write!(formatter, "fixed read operation failed (status {code:?})")
            }
            Self::MalformedOutput => write!(formatter, "ocserv output format unsupported"),
            Self::Io(error) => write!(formatter, "fixed local resource failed: {error}"),
        }
    }
}

impl std::error::Error for AdapterError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Io(error) | Self::ResourceIo { source: error, .. } => Some(error),
            _ => None,
        }
    }
}

impl From<AdapterError> for PrivdError {
    fn from(error: AdapterError) -> Self {
        let kind = match &error {
            AdapterError::InvalidResource
            | AdapterError::MalformedOutput
            | AdapterError::ResourceMetadata { .. }
            | AdapterError::UserFileFormat { .. } => ErrorKind::MalformedOutput,
            AdapterError::InvalidRequest | AdapterError::StaleBoot => ErrorKind::InvalidRequest,
            AdapterError::Unavailable | AdapterError::Io(_) | AdapterError::ResourceIo { .. } => {
                ErrorKind::Unavailable
            }
            AdapterError::DeadlineExceeded => ErrorKind::DeadlineExceeded,
            AdapterError::OutputLimit => ErrorKind::OutputLimit,
            AdapterError::CapacityExceeded => ErrorKind::CapacityExceeded,
            AdapterError::CommandFailed { .. } => ErrorKind::CommandFailed,
        };
        Self {
            kind: kind.into(),
            detail: error.to_string().chars().take(256).collect(),
        }
    }
}

fn utf8(bytes: &[u8]) -> Result<&str, AdapterError> {
    std::str::from_utf8(bytes).map_err(|_| AdapterError::MalformedOutput)
}

/// Parses the fixed `systemctl show` output.
///
/// # Errors
///
/// Returns `MalformedOutput` unless all fixed fields are present and valid.
pub fn parse_service_status(bytes: &[u8]) -> Result<ServiceStatus, AdapterError> {
    let mut values = HashMap::new();
    for line in utf8(bytes)?.lines().filter(|line| !line.is_empty()) {
        let (key, value) = line.split_once('=').ok_or(AdapterError::MalformedOutput)?;
        if values.insert(key, value).is_some() || value.len() > 64 {
            return Err(AdapterError::MalformedOutput);
        }
    }
    Ok(ServiceStatus {
        load_state: required_value(&values, "LoadState")?,
        active_state: required_value(&values, "ActiveState")?,
        sub_state: required_value(&values, "SubState")?,
    })
}

fn required_value(values: &HashMap<&str, &str>, key: &str) -> Result<String, AdapterError> {
    let value = values.get(key).ok_or(AdapterError::MalformedOutput)?;
    if value.is_empty()
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte == b'-')
    {
        return Err(AdapterError::MalformedOutput);
    }
    Ok((*value).to_owned())
}

/// Parses supported Ocserv version output.
///
/// # Errors
///
/// Returns `MalformedOutput` for unsupported or invalid version text.
pub fn parse_version(bytes: &[u8]) -> Result<OcservVersion, AdapterError> {
    let text = utf8(bytes)?.trim();
    if text.len() > 4096 {
        return Err(AdapterError::MalformedOutput);
    }
    let first_line = text.lines().next().ok_or(AdapterError::MalformedOutput)?;
    let version = first_line
        .strip_prefix("ocserv ")
        .or_else(|| first_line.strip_prefix("OpenConnect VPN Server "))
        .ok_or(AdapterError::MalformedOutput)?;
    if !(version.starts_with("1.2.")
        || version.starts_with("1.3.")
        || version.starts_with("1.4.")
        || version == "1.5.0"
        || version == "1.6.0")
        || version.len() > 32
        || !version
            .bytes()
            .all(|byte| byte.is_ascii_digit() || byte == b'.')
    {
        return Err(AdapterError::MalformedOutput);
    }
    Ok(OcservVersion {
        version: version.to_owned(),
    })
}

/// Parses the versioned `occtl --json show users` format.
///
/// # Errors
///
/// Returns `MalformedOutput` for invalid headers, fields, addresses, or bounds.
pub fn parse_sessions(bytes: &[u8]) -> Result<SessionList, AdapterError> {
    let value: serde_json::Value =
        serde_json::from_slice(bytes).map_err(|_| AdapterError::MalformedOutput)?;
    let items = value.as_array().ok_or(AdapterError::MalformedOutput)?;
    if items.len() > 4096 {
        return Err(AdapterError::MalformedOutput);
    }
    let mut sessions = Vec::new();
    for item in items {
        let object = item.as_object().ok_or(AdapterError::MalformedOutput)?;
        let id = object
            .get("ID")
            .and_then(serde_json::Value::as_u64)
            .filter(|id| *id > 0)
            .ok_or(AdapterError::MalformedOutput)?
            .to_string();
        let username = validated_token(json_string(object, "Username")?, 128)?;
        let remote_ip = canonical_ip(json_string(object, "Remote IP")?)?;
        let vpn_ip = object
            .get("IPv4")
            .or_else(|| object.get("IPv6"))
            .and_then(serde_json::Value::as_str)
            .ok_or(AdapterError::MalformedOutput)
            .and_then(canonical_ip)?;
        sessions.push(Session {
            id,
            username,
            remote_ip,
            vpn_ip,
        });
    }
    Ok(SessionList { sessions })
}

/// Parses the versioned `occtl --json show ip bans` format.
///
/// # Errors
///
/// Returns `MalformedOutput` for invalid headers, fields, addresses, or bounds.
pub fn parse_ip_bans(bytes: &[u8]) -> Result<IpBanList, AdapterError> {
    let value: serde_json::Value =
        serde_json::from_slice(bytes).map_err(|_| AdapterError::MalformedOutput)?;
    let items = value.as_array().ok_or(AdapterError::MalformedOutput)?;
    if items.len() > 4096 {
        return Err(AdapterError::MalformedOutput);
    }
    let mut bans = Vec::new();
    for item in items {
        let object = item.as_object().ok_or(AdapterError::MalformedOutput)?;
        bans.push(IpBan {
            ip: canonical_ip(json_string(object, "IP")?)?,
            seconds_remaining: None,
        });
    }
    Ok(IpBanList { bans })
}

fn json_string<'a>(
    object: &'a serde_json::Map<String, serde_json::Value>,
    name: &str,
) -> Result<&'a str, AdapterError> {
    object
        .get(name)
        .and_then(serde_json::Value::as_str)
        .ok_or(AdapterError::MalformedOutput)
}

fn validated_token(value: &str, limit: usize) -> Result<String, AdapterError> {
    if value.is_empty() || value.len() > limit || value.chars().any(char::is_control) {
        return Err(AdapterError::MalformedOutput);
    }
    Ok(value.to_owned())
}

fn canonical_ip(value: &str) -> Result<String, AdapterError> {
    value
        .parse::<IpAddr>()
        .map(|ip| ip.to_string())
        .map_err(|_| AdapterError::MalformedOutput)
}

fn numeric_session_id(value: &str) -> Result<&str, AdapterError> {
    let parsed = value
        .parse::<u64>()
        .map_err(|_| AdapterError::InvalidRequest)?;
    if parsed == 0 || parsed.to_string() != value {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(value)
}

#[cfg(test)]
mod tests;
