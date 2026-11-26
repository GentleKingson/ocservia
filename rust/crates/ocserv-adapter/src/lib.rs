//! Version-aware Ocserv adapter with fixed resources, executables, and arguments.

#![forbid(unsafe_code)]

mod certificates;
mod complete_config;
mod configuration;
mod effect_store;
mod parsing;
mod user_file;
use effect_store::{
    EffectStore, PrepareEffect, effect_now, reject_symlink, validate_effect_identity,
};
use parsing::canonical_ip;
pub use parsing::{
    AdapterError, parse_ip_bans, parse_service_status, parse_sessions, parse_version,
};
use user_file::authoritative_user_owner;
pub use user_file::parse_user_file;

use std::collections::{HashMap, HashSet};
use std::ffi::OsString;
use std::fs::{File, OpenOptions as StdOpenOptions};
use std::io::{self, Write};
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
    DesiredEffectState, GroupList, IpBanList, MAX_FRAME_BYTES, MAX_MANAGED_RESOURCES,
    MutationResult, ObservedGroup, ObservedUser, OcservVersion, ServiceStatus, SessionList,
    UserList,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    SealedSecretPurpose, SealedSecretV1,
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
const MAX_ARTIFACT_BYTES: usize = 256 * 1024;
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
    certificate_key_dir: PathBuf,
    config_tls_root: PathBuf,
    audit_hook: PathBuf,
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
            certificate_key_dir: PathBuf::from("/var/lib/ocservia-privd/certificates"),
            config_tls_root: PathBuf::from("/etc/ocservia-agent/config-tls"),
            audit_hook: PathBuf::from("/usr/libexec/ocservia/vpn-session-hook"),
        }
    }
}

impl FixedResources {
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

/// Root-verified connection-owner term attached to a privileged operation.
/// The node and endpoint are pinned by privd rather than trusted from Agent
/// input.
#[derive(Clone, Copy, Debug)]
pub struct OwnerFenceIdentity<'a> {
    pub node_id: &'a [u8],
    pub endpoint_id: &'a [u8],
    pub owner_epoch: u64,
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
    pub owner_fence: OwnerFenceIdentity<'a>,
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
    effect_store: Arc<std::sync::Mutex<Option<EffectStore>>>,
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
            effect_store: Arc::new(std::sync::Mutex::new(None)),
            #[cfg(test)]
            config_apply_fault: Arc::new(AtomicU8::new(0)),
        }
    }

    pub(crate) fn with_effect_store<T>(
        &self,
        allow_initialize: bool,
        run: impl FnOnce(&mut EffectStore) -> Result<T, AdapterError>,
    ) -> Result<T, AdapterError> {
        let mut slot = self
            .effect_store
            .lock()
            .map_err(|_| AdapterError::Unavailable)?;
        if slot.is_none() {
            let store = if allow_initialize {
                EffectStore::open_for_mutation(&self.resources)?
            } else {
                EffectStore::open_existing(&self.resources)?
            };
            *slot = Some(store);
        }
        let store = slot.as_mut().ok_or(AdapterError::Unavailable)?;
        let result = run(store);
        if matches!(result, Err(AdapterError::Io(_))) {
            *slot = None;
        }
        result
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
        owner_fence: OwnerFenceIdentity<'_>,
    ) -> Result<AuthorizedEffectDecision, AdapterError> {
        self.with_effect_store(true, |store| {
            store.prepare_authorized(identity, owner_fence)
        })
    }

    /// Durably accepts a root-verified connection-owner term before any
    /// operation-specific decision. Effect and artifact methods recheck the
    /// same floor in their own state transactions to close concurrent races.
    ///
    /// # Errors
    ///
    /// Rejects malformed or stale terms and unavailable or unauthenticated
    /// effect-store state.
    pub fn accept_owner_fence(
        &self,
        owner_fence: OwnerFenceIdentity<'_>,
    ) -> Result<(), AdapterError> {
        self.with_effect_store(true, |store| store.accept_owner_fence(owner_fence))
    }

    /// Reads an exact authenticated terminal response only after atomically
    /// accepting the signed connection-owner term. It never creates or retries
    /// a privileged effect.
    ///
    /// # Errors
    ///
    /// Rejects stale owner terms, identity conflicts, corrupted records, and
    /// unavailable storage.
    pub fn replay_authorized_effect_after_fence(
        &self,
        identity: AuthorizedEffectIdentity<'_>,
        owner_fence: OwnerFenceIdentity<'_>,
    ) -> Result<Option<Vec<u8>>, AdapterError> {
        self.with_effect_store(true, |store| {
            store.replay_authorized_after_fence(identity, owner_fence)
        })
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
        self.with_effect_store(false, |store| store.complete_authorized(identity, response))
    }

    /// Allocates the next durable, HMAC-authenticated monotonic receipt
    /// sequence number for a newly settled privileged effect.
    ///
    /// # Errors
    ///
    /// Rejects uninitialized, tampered, or unavailable effect stores.
    pub fn next_receipt_sequence(&self) -> Result<u64, AdapterError> {
        self.with_effect_store(true, EffectStore::next_receipt_sequence)
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
        self.with_effect_store(false, |store| store.replay_authorized(identity))
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
    /// Returns the first failing check's name and its bounded resource or parser error.
    pub async fn host_preflight(&self) -> Result<(), (&'static str, AdapterError)> {
        self.user_list()
            .await
            .map_err(|error| ("ocpasswd.users", error))?;
        self.group_list()
            .await
            .map_err(|error| ("ocpasswd.groups", error))?;
        self.ocserv_version()
            .await
            .map_err(|error| ("ocserv.version", error))?;
        self.config_fingerprint()
            .await
            .map_err(|error| ("ocserv.config-fingerprint", error))?;
        self.service_status()
            .await
            .map_err(|error| ("systemctl.ocserv-service-status", error))?;
        let status = self
            .execute(&self.resources.occtl, &["--json", "show", "status"])
            .await
            .map_err(|error| ("occtl.show-status", error))?;
        let status: serde_json::Value = serde_json::from_slice(&status.stdout)
            .map_err(|_| ("occtl.show-status", AdapterError::MalformedOutput))?;
        if status.get("Status").and_then(serde_json::Value::as_str) != Some("online") {
            return Err(("occtl.show-status", AdapterError::Unavailable));
        }
        self.session_list()
            .await
            .map_err(|error| ("occtl.show-users-json", error))?;
        self.ip_ban_list()
            .await
            .map_err(|error| ("occtl.show-ip-bans-json", error))?;
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
        if program == self.resources.occtl {
            command.env("TZ", "UTC");
        }
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

    pub(crate) async fn decrypt_sealed_password(
        &self,
        key_path: &Path,
        ciphertext: &[u8],
    ) -> Result<Zeroizing<Vec<u8>>, AdapterError> {
        let key = key_path.to_str().ok_or(AdapterError::InvalidResource)?;
        let output = self
            .execute_with_input(
                &self.resources.openssl,
                &[
                    "pkeyutl",
                    "-decrypt",
                    "-inkey",
                    key,
                    "-pkeyopt",
                    "rsa_padding_mode:oaep",
                    "-pkeyopt",
                    "rsa_oaep_md:sha256",
                ],
                ciphertext,
            )
            .await?;
        let password = Zeroizing::new(output.stdout);
        if password.is_empty()
            || password.len() > 1024
            || password
                .iter()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
        {
            return Err(AdapterError::MalformedOutput);
        }
        Ok(password)
    }
}

fn validate_name(value: &str) -> Result<(), AdapterError> {
    if !ocservia_contracts::unix::is_valid_resource_name(value) {
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
