//! Version-aware Ocserv adapter with fixed resources, executables, and arguments.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet};
use std::io;
use std::net::IpAddr;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use ocservia_agent_protocol::{
    ConfigFingerprint, DesiredEffectObservation, ErrorKind, GroupList, IpBan, IpBanList,
    MutationResult, ObservedGroup, ObservedUser, OcservVersion, PrivdError, ServiceStatus, Session,
    SessionList, UserList,
};
use rustix::fs::XattrFlags;
use sha2::{Digest, Sha256};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::process::Command;
use tokio::sync::Mutex;
use uuid::Uuid;
use zeroize::Zeroizing;

const MAX_CONFIG_BYTES: usize = 1024 * 1024;
const DEFAULT_OUTPUT_BYTES: usize = 256 * 1024;
const EFFECT_XATTR_PREFIX: &str = "user.ocservia.effect.";
const MAX_MANAGED_XATTR_BYTES: usize = 64 * 1024;

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
    secret_key: PathBuf,
    secret_key_id: String,
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
            secret_key: PathBuf::from("/etc/ocservia/password-seal-private.pem"),
            secret_key_id: String::from("default"),
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
        self.secret_key = secret_key;
        self.secret_key_id = secret_key_id;
        Ok(self)
    }
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
}

impl Adapter {
    /// Creates an adapter with fixed trusted resources.
    #[must_use]
    pub fn new(resources: FixedResources, limits: Limits) -> Self {
        Self {
            resources,
            limits,
            user_file_lock: Arc::new(Mutex::new(())),
        }
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

    /// Fingerprints the fixed Ocserv config with a strict size bound.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for missing, unreadable, or oversized data.
    pub async fn config_fingerprint(&self) -> Result<ConfigFingerprint, AdapterError> {
        let file = tokio::fs::File::open(&self.resources.config)
            .await
            .map_err(AdapterError::Io)?;
        let mut bytes = Vec::with_capacity(4096);
        file.take(u64::try_from(MAX_CONFIG_BYTES + 1).map_err(|_| AdapterError::OutputLimit)?)
            .read_to_end(&mut bytes)
            .await
            .map_err(AdapterError::Io)?;
        if bytes.len() > MAX_CONFIG_BYTES {
            return Err(AdapterError::OutputLimit);
        }
        Ok(ConfigFingerprint {
            sha256: hex::encode(Sha256::digest(&bytes)),
            size_bytes: u64::try_from(bytes.len()).map_err(|_| AdapterError::OutputLimit)?,
        })
    }

    /// Lists fixed ocpasswd identities without returning password hashes.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for unreadable or malformed data.
    pub async fn user_list(&self) -> Result<UserList, AdapterError> {
        let bytes = read_optional_secret_file(&self.resources.user_file).await?;
        parse_user_file(&bytes)
    }

    /// Lists group membership from the group field in the fixed ocpasswd file.
    ///
    /// # Errors
    ///
    /// Returns a stable adapter error for unreadable or malformed data.
    pub async fn group_list(&self) -> Result<GroupList, AdapterError> {
        let bytes = read_optional_secret_file(&self.resources.user_file).await?;
        parse_groups_from_user_file(&bytes)
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
    /// Rejects a malformed or stale target and propagates bounded child failures.
    pub async fn session_terminate(
        &self,
        session_id: &str,
        boot_id: &str,
    ) -> Result<MutationResult, AdapterError> {
        self.ensure_boot_id(boot_id).await?;
        let session_id = numeric_session_id(session_id)?;
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

    /// Creates a user only when the authoritative password file has no matching record.
    ///
    /// # Errors
    ///
    /// Rejects invalid inputs and propagates bounded fixed-command failures.
    pub async fn user_create(
        &self,
        username: &str,
        key_id: &str,
        sealed_password: &[u8],
        desired_revision: u64,
    ) -> Result<MutationResult, AdapterError> {
        self.user_secret_apply(
            username,
            key_id,
            sealed_password,
            desired_revision,
            SecretApplyMode::MustBeAbsent,
        )
        .await
    }

    /// Rotates a password only when the authoritative password file contains the user.
    ///
    /// # Errors
    ///
    /// Rejects missing users and propagates bounded fixed-command failures.
    pub async fn user_password_rotate(
        &self,
        username: &str,
        key_id: &str,
        sealed_password: &[u8],
        desired_revision: u64,
    ) -> Result<MutationResult, AdapterError> {
        self.user_secret_apply(
            username,
            key_id,
            sealed_password,
            desired_revision,
            SecretApplyMode::MustExist,
        )
        .await
    }

    async fn user_secret_apply(
        &self,
        username: &str,
        key_id: &str,
        sealed_password: &[u8],
        desired_revision: u64,
        mode: SecretApplyMode,
    ) -> Result<MutationResult, AdapterError> {
        validate_name(username)?;
        if !valid_key_id(key_id)
            || key_id != self.resources.secret_key_id
            || sealed_password.len() < 32
            || sealed_password.len() > 4096
            || desired_revision == 0
        {
            return Err(AdapterError::InvalidRequest);
        }
        let _guard = self.user_file_lock.lock().await;
        let current = read_optional_secret_file(&self.resources.user_file).await?;
        let metadata = find_user_metadata(&current, username)?;
        if (mode == SecretApplyMode::MustBeAbsent && metadata.is_some())
            || (mode == SecretApplyMode::MustExist && metadata.is_none())
        {
            return Err(AdapterError::InvalidRequest);
        }
        let key = self
            .resources
            .secret_key
            .to_str()
            .ok_or(AdapterError::InvalidResource)?;
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
                sealed_password,
            )
            .await?;
        let mut password = Zeroizing::new(output.stdout);
        if password.is_empty()
            || password.len() > 1024
            || password
                .iter()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
        {
            return Err(AdapterError::MalformedOutput);
        }
        let mut input = Zeroizing::new(Vec::with_capacity(password.len() * 2 + 2));
        input.extend_from_slice(&password);
        input.push(b'\n');
        input.extend_from_slice(&password);
        input.push(b'\n');
        password.fill(0);
        let staging = self.stage_user_file(&current).await?;
        let staging_text = staging.to_str().ok_or(AdapterError::InvalidResource)?;
        let result = async {
            let mut args = vec!["-c", staging_text];
            if let Some((groups, _)) = metadata.as_ref() {
                args.extend(["-g", groups.as_str()]);
            }
            args.push(username);
            self.execute_with_input(&self.resources.ocpasswd, &args, &input)
                .await?;
            if metadata.as_ref().is_some_and(|(_, disabled)| *disabled) {
                self.execute(
                    &self.resources.ocpasswd,
                    &["-c", staging_text, "-l", username],
                )
                .await?;
            }
            // ocpasswd may replace its target inode, so restore markers from the
            // still-authoritative file before committing the staged result.
            copy_effect_markers(&self.resources.user_file, &staging)?;
            set_effect_marker(&staging, mode.mutation_kind(), username, desired_revision)?;
            commit_staging(&staging, &self.resources.user_file).await
        }
        .await;
        if result.is_err() {
            let _ = tokio::fs::remove_file(&staging).await;
        }
        result?;
        Ok(MutationResult { applied: true })
    }

    /// Disables exactly one validated user in the fixed password file.
    ///
    /// # Errors
    ///
    /// Rejects invalid names and propagates bounded fixed-command failures.
    pub async fn user_disable(
        &self,
        username: &str,
        desired_revision: u64,
    ) -> Result<MutationResult, AdapterError> {
        self.user_lock_state(username, desired_revision, true).await
    }

    /// Enables exactly one validated user without changing its password or groups.
    ///
    /// # Errors
    ///
    /// Rejects invalid names and propagates bounded fixed-command failures.
    pub async fn user_enable(
        &self,
        username: &str,
        desired_revision: u64,
    ) -> Result<MutationResult, AdapterError> {
        self.user_lock_state(username, desired_revision, false)
            .await
    }

    async fn user_lock_state(
        &self,
        username: &str,
        desired_revision: u64,
        locked: bool,
    ) -> Result<MutationResult, AdapterError> {
        validate_name(username)?;
        if desired_revision == 0 {
            return Err(AdapterError::InvalidRequest);
        }
        let _guard = self.user_file_lock.lock().await;
        let current = read_optional_secret_file(&self.resources.user_file).await?;
        if find_user_metadata(&current, username)?.is_none() {
            return Err(AdapterError::InvalidRequest);
        }
        let staging = self.stage_user_file(&current).await?;
        let staging_text = staging.to_str().ok_or(AdapterError::InvalidResource)?;
        let action = if locked { "-l" } else { "-u" };
        let result = async {
            self.execute(
                &self.resources.ocpasswd,
                &["-c", staging_text, action, username],
            )
            .await?;
            let mutation_kind = if locked {
                "user_disable"
            } else {
                "user_enable"
            };
            copy_effect_markers(&self.resources.user_file, &staging)?;
            set_effect_marker(&staging, mutation_kind, username, desired_revision)?;
            commit_staging(&staging, &self.resources.user_file).await
        }
        .await;
        if result.is_err() {
            let _ = tokio::fs::remove_file(&staging).await;
        }
        result?;
        Ok(MutationResult { applied: true })
    }

    /// Atomically replaces one group across Ocserv's authoritative ocpasswd records.
    ///
    /// # Errors
    ///
    /// Rejects noncanonical inputs and propagates bounded file failures.
    pub async fn group_apply(
        &self,
        group_name: &str,
        members: &[String],
        desired_revision: u64,
    ) -> Result<MutationResult, AdapterError> {
        validate_name(group_name)?;
        if members.len() > 4096 || desired_revision == 0 {
            return Err(AdapterError::InvalidRequest);
        }
        let mut previous = "";
        for member in members {
            validate_name(member)?;
            if member.as_str() <= previous {
                return Err(AdapterError::InvalidRequest);
            }
            previous = member;
        }
        let _guard = self.user_file_lock.lock().await;
        let bytes = read_optional_secret_file(&self.resources.user_file).await?;
        let records = parse_secret_user_records(&bytes)?;
        let requested: HashSet<&str> = members.iter().map(String::as_str).collect();
        let existing: HashSet<&str> = records
            .iter()
            .map(|record| record.username.as_str())
            .collect();
        if !requested.is_subset(&existing) {
            return Err(AdapterError::InvalidRequest);
        }
        let mut output = Zeroizing::new(Vec::with_capacity(bytes.len().saturating_add(128)));
        for record in records {
            let mut groups: Vec<String> = record
                .groups
                .split(',')
                .filter(|group| {
                    !group.is_empty() && *group != "*" && *group != "x" && *group != group_name
                })
                .map(str::to_owned)
                .collect();
            if requested.contains(record.username.as_str()) {
                groups.push(group_name.to_owned());
            }
            groups.sort();
            groups.dedup();
            let group_field = if groups.is_empty() {
                "*".to_owned()
            } else {
                groups.join(",")
            };
            output.extend_from_slice(record.username.as_bytes());
            output.push(b':');
            output.extend_from_slice(group_field.as_bytes());
            output.push(b':');
            output.extend_from_slice(record.hash.as_bytes());
            output.push(b'\n');
        }
        atomic_replace(
            &self.resources.user_file,
            &output,
            Some(("group_apply", group_name, desired_revision)),
        )
        .await?;
        Ok(MutationResult { applied: true })
    }

    /// Checks the non-secret effect marker committed with an authoritative file replacement.
    ///
    /// # Errors
    ///
    /// Rejects unknown mutation kinds or invalid resource identities.
    pub async fn desired_effect_observe(
        &self,
        mutation_kind: &str,
        resource_key: &str,
        desired_revision: u64,
    ) -> Result<DesiredEffectObservation, AdapterError> {
        validate_effect_identity(mutation_kind, resource_key, desired_revision)?;
        let _guard = self.user_file_lock.lock().await;
        let applied = effect_marker_matches(
            &self.resources.user_file,
            mutation_kind,
            resource_key,
            desired_revision,
        )?;
        Ok(DesiredEffectObservation { applied })
    }

    async fn stage_user_file(&self, bytes: &[u8]) -> Result<PathBuf, AdapterError> {
        let path = &self.resources.user_file;
        let parent = path.parent().ok_or(AdapterError::InvalidResource)?;
        let name = path
            .file_name()
            .and_then(|value| value.to_str())
            .ok_or(AdapterError::InvalidResource)?;
        let staging = parent.join(format!(".{name}.ocservia-{}", Uuid::now_v7()));
        let mode = file_mode(path).await?;
        let mut options = tokio::fs::OpenOptions::new();
        options.write(true).create_new(true).mode(mode);
        let mut file = options.open(&staging).await.map_err(AdapterError::Io)?;
        if let Err(error) = async {
            file.write_all(bytes).await.map_err(AdapterError::Io)?;
            file.sync_all().await.map_err(AdapterError::Io)?;
            copy_effect_markers(path, &staging)
        }
        .await
        {
            let _ = tokio::fs::remove_file(&staging).await;
            return Err(error);
        }
        Ok(staging)
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
        if !input.is_empty() {
            let mut stdin = child.stdin.take().ok_or(AdapterError::Unavailable)?;
            stdin.write_all(input).await.map_err(AdapterError::Io)?;
            stdin.shutdown().await.map_err(AdapterError::Io)?;
        }
        let stdout = child.stdout.take().ok_or(AdapterError::Unavailable)?;
        let stderr = child.stderr.take().ok_or(AdapterError::Unavailable)?;
        let stdout_task = tokio::spawn(read_bounded(stdout, self.limits.output_bytes));
        let stderr_task = tokio::spawn(read_bounded(stderr, self.limits.output_bytes));
        let status =
            if let Ok(result) = tokio::time::timeout(self.limits.timeout, child.wait()).await {
                result.map_err(AdapterError::Io)?
            } else {
                kill_process_group(&child);
                let _ = child.wait().await;
                return Err(AdapterError::DeadlineExceeded);
            };
        let stdout = stdout_task.await.map_err(|_| AdapterError::Unavailable)??;
        let stderr = stderr_task.await.map_err(|_| AdapterError::Unavailable)??;
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

/// Parses a bounded ocpasswd file while discarding every hash.
///
/// # Errors
///
/// Rejects malformed lines, unsafe names, and oversized input.
pub fn parse_user_file(bytes: &[u8]) -> Result<UserList, AdapterError> {
    let mut users: Vec<_> = parse_secret_user_records(bytes)?
        .into_iter()
        .map(|record| ObservedUser {
            username: record.username,
            enabled: !record.hash.starts_with('!'),
        })
        .collect();
    users.sort_by(|left, right| left.username.cmp(&right.username));
    Ok(UserList { users })
}

fn parse_groups_from_user_file(bytes: &[u8]) -> Result<GroupList, AdapterError> {
    let records = parse_secret_user_records(bytes)?;
    let mut grouped: HashMap<String, Vec<String>> = HashMap::new();
    for record in records {
        for group in record
            .groups
            .split(',')
            .filter(|group| !group.is_empty() && !matches!(*group, "*" | "x"))
        {
            grouped
                .entry(group.to_owned())
                .or_default()
                .push(record.username.clone());
        }
    }
    let mut groups: Vec<_> = grouped
        .into_iter()
        .map(|(group_name, mut members)| {
            members.sort();
            ObservedGroup {
                group_name,
                members,
            }
        })
        .collect();
    groups.sort_by(|left, right| left.group_name.cmp(&right.group_name));
    Ok(GroupList { groups })
}

#[derive(Debug)]
struct SecretUserRecord {
    username: String,
    groups: String,
    hash: Zeroizing<String>,
}

fn parse_secret_user_records(bytes: &[u8]) -> Result<Vec<SecretUserRecord>, AdapterError> {
    if bytes.len() > MAX_CONFIG_BYTES {
        return Err(AdapterError::OutputLimit);
    }
    let mut records = Vec::new();
    for line in utf8(bytes)?.lines().filter(|line| !line.is_empty()) {
        let mut parts = line.split(':');
        let username = parts.next().ok_or(AdapterError::MalformedOutput)?;
        let groups = parts.next().ok_or(AdapterError::MalformedOutput)?;
        let hash = parts.next().ok_or(AdapterError::MalformedOutput)?;
        if parts.next().is_some() || hash.is_empty() {
            return Err(AdapterError::MalformedOutput);
        }
        validate_name(username).map_err(|_| AdapterError::MalformedOutput)?;
        for group in groups.split(',').filter(|group| !group.is_empty()) {
            if matches!(group, "*" | "x") {
                continue;
            }
            validate_name(group).map_err(|_| AdapterError::MalformedOutput)?;
        }
        records.push(SecretUserRecord {
            username: username.to_owned(),
            groups: groups.to_owned(),
            hash: Zeroizing::new(hash.to_owned()),
        });
    }
    records.sort_by(|left, right| left.username.cmp(&right.username));
    if records
        .windows(2)
        .any(|pair| pair[0].username == pair[1].username)
    {
        return Err(AdapterError::MalformedOutput);
    }
    Ok(records)
}

fn find_user_metadata(
    bytes: &[u8],
    username: &str,
) -> Result<Option<(String, bool)>, AdapterError> {
    Ok(parse_secret_user_records(bytes)?
        .into_iter()
        .find(|record| record.username == username)
        .map(|record| (record.groups, record.hash.starts_with('!'))))
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum SecretApplyMode {
    MustBeAbsent,
    MustExist,
}

impl SecretApplyMode {
    const fn mutation_kind(self) -> &'static str {
        match self {
            Self::MustBeAbsent => "user_create",
            Self::MustExist => "user_password_rotate",
        }
    }
}

fn validate_effect_identity(
    mutation_kind: &str,
    resource_key: &str,
    desired_revision: u64,
) -> Result<(), AdapterError> {
    if !matches!(
        mutation_kind,
        "user_create" | "user_disable" | "user_enable" | "user_password_rotate" | "group_apply"
    ) || desired_revision == 0
    {
        return Err(AdapterError::InvalidRequest);
    }
    validate_name(resource_key)
}

fn effect_marker_name(mutation_kind: &str, resource_key: &str) -> String {
    let mut hash = Sha256::new();
    hash.update(b"ocservia.desired-effect-resource.v1\0");
    hash.update(mutation_kind.as_bytes());
    hash.update([0]);
    hash.update(resource_key.as_bytes());
    format!("{EFFECT_XATTR_PREFIX}{}", hex::encode(hash.finalize()))
}

fn effect_marker_value(mutation_kind: &str, resource_key: &str, desired_revision: u64) -> [u8; 32] {
    let mut hash = Sha256::new();
    hash.update(b"ocservia.desired-effect-marker.v1\0");
    hash.update(mutation_kind.as_bytes());
    hash.update([0]);
    hash.update(resource_key.as_bytes());
    hash.update([0]);
    hash.update(desired_revision.to_be_bytes());
    hash.finalize().into()
}

fn set_effect_marker(
    path: &Path,
    mutation_kind: &str,
    resource_key: &str,
    desired_revision: u64,
) -> Result<(), AdapterError> {
    validate_effect_identity(mutation_kind, resource_key, desired_revision)?;
    rustix::fs::setxattr(
        path,
        effect_marker_name(mutation_kind, resource_key),
        &effect_marker_value(mutation_kind, resource_key, desired_revision),
        XattrFlags::empty(),
    )
    .map_err(rustix_io)
}

fn effect_marker_matches(
    path: &Path,
    mutation_kind: &str,
    resource_key: &str,
    desired_revision: u64,
) -> Result<bool, AdapterError> {
    let name = effect_marker_name(mutation_kind, resource_key);
    let mut value = vec![0_u8; 64];
    match rustix::fs::getxattr(path, name, &mut value) {
        Ok(length) => {
            Ok(value[..length]
                == effect_marker_value(mutation_kind, resource_key, desired_revision))
        }
        Err(rustix::io::Errno::NOENT | rustix::io::Errno::NODATA) => Ok(false),
        Err(error) => Err(rustix_io(error)),
    }
}

fn copy_effect_markers(source: &Path, destination: &Path) -> Result<(), AdapterError> {
    if !source.exists() {
        return Ok(());
    }
    let mut names = vec![0_u8; MAX_MANAGED_XATTR_BYTES];
    let names_length = rustix::fs::listxattr(source, &mut names).map_err(rustix_io)?;
    for raw_name in names[..names_length]
        .split(|byte| *byte == 0)
        .filter(|name| !name.is_empty())
    {
        let name = std::str::from_utf8(raw_name).map_err(|_| AdapterError::InvalidResource)?;
        if !name.starts_with(EFFECT_XATTR_PREFIX) {
            continue;
        }
        let mut value = vec![0_u8; 64];
        let value_length = rustix::fs::getxattr(source, name, &mut value).map_err(rustix_io)?;
        if value_length != 32 {
            return Err(AdapterError::InvalidResource);
        }
        rustix::fs::setxattr(
            destination,
            name,
            &value[..value_length],
            XattrFlags::empty(),
        )
        .map_err(rustix_io)?;
    }
    Ok(())
}

fn rustix_io(error: rustix::io::Errno) -> AdapterError {
    AdapterError::Io(io::Error::from_raw_os_error(error.raw_os_error()))
}

async fn read_optional_secret_file(path: &Path) -> Result<Zeroizing<Vec<u8>>, AdapterError> {
    match tokio::fs::read(path).await {
        Ok(value) => Ok(Zeroizing::new(value)),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(Zeroizing::new(Vec::new())),
        Err(error) => Err(AdapterError::Io(error)),
    }
}

async fn atomic_replace(
    path: &Path,
    bytes: &[u8],
    effect: Option<(&str, &str, u64)>,
) -> Result<(), AdapterError> {
    let parent = path.parent().ok_or(AdapterError::InvalidResource)?;
    let name = path
        .file_name()
        .and_then(|value| value.to_str())
        .ok_or(AdapterError::InvalidResource)?;
    let staging = parent.join(format!(".{name}.ocservia-{}", Uuid::now_v7()));
    let mode = file_mode(path).await?;
    let mut options = tokio::fs::OpenOptions::new();
    options.write(true).create_new(true).mode(mode);
    let mut file = options.open(&staging).await.map_err(AdapterError::Io)?;
    let result = async {
        file.write_all(bytes).await.map_err(AdapterError::Io)?;
        file.sync_all().await.map_err(AdapterError::Io)?;
        copy_effect_markers(path, &staging)?;
        if let Some((mutation_kind, resource_key, desired_revision)) = effect {
            set_effect_marker(&staging, mutation_kind, resource_key, desired_revision)?;
        }
        commit_staging(&staging, path).await
    }
    .await;
    if result.is_err() {
        let _ = tokio::fs::remove_file(&staging).await;
    }
    result
}

async fn file_mode(path: &Path) -> Result<u32, AdapterError> {
    match tokio::fs::metadata(path).await {
        Ok(metadata) => Ok(metadata.permissions().mode() & 0o777),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(0o660),
        Err(error) => Err(AdapterError::Io(error)),
    }
}

async fn commit_staging(staging: &Path, path: &Path) -> Result<(), AdapterError> {
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
    let directory = tokio::fs::File::open(parent)
        .await
        .map_err(AdapterError::Io)?;
    directory.sync_all().await.map_err(AdapterError::Io)
}

fn kill_process_group(child: &tokio::process::Child) {
    let Some(raw_pid) = child.id().and_then(|pid| i32::try_from(pid).ok()) else {
        return;
    };
    let Some(pid) = rustix::process::Pid::from_raw(raw_pid) else {
        return;
    };
    let _ = rustix::process::kill_process_group(pid, rustix::process::Signal::KILL);
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
            Self::InvalidRequest => write!(formatter, "fixed operation argument invalid"),
            Self::StaleBoot => write!(formatter, "session boot identity is stale"),
            Self::Unavailable => write!(formatter, "fixed local resource unavailable"),
            Self::DeadlineExceeded => write!(formatter, "fixed read operation timed out"),
            Self::OutputLimit => write!(formatter, "fixed read operation output limit exceeded"),
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
            Self::Io(error) => Some(error),
            _ => None,
        }
    }
}

impl From<AdapterError> for PrivdError {
    fn from(error: AdapterError) -> Self {
        let kind = match &error {
            AdapterError::InvalidResource | AdapterError::MalformedOutput => {
                ErrorKind::MalformedOutput
            }
            AdapterError::InvalidRequest | AdapterError::StaleBoot => ErrorKind::InvalidRequest,
            AdapterError::Unavailable | AdapterError::Io(_) => ErrorKind::Unavailable,
            AdapterError::DeadlineExceeded => ErrorKind::DeadlineExceeded,
            AdapterError::OutputLimit => ErrorKind::OutputLimit,
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

/// Parses supported Ocserv 1.2 and 1.3 version output.
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
    if !(version.starts_with("1.2.") || version.starts_with("1.3.") || version.starts_with("1.4."))
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
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use super::*;

    fn executable(name: &str, body: &str) -> PathBuf {
        let directory =
            std::env::temp_dir().join(format!("ocservia-adapter-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir_all(&directory).expect("create fixture directory");
        let path = directory.join(name);
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).expect("write fixture program");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700))
            .expect("make fixture executable");
        path
    }

    #[test]
    fn version_fixtures_and_malformed_output() {
        assert_eq!(
            parse_version(b"ocserv 1.2.4\n")
                .expect("1.2 fixture")
                .version,
            "1.2.4"
        );
        assert_eq!(
            parse_version(
                b"OpenConnect VPN Server 1.3.0\n\nCompiled with: seccomp, PAM\nGnuTLS version: 3.8.9\n"
            )
                .expect("1.3 fixture")
                .version,
            "1.3.0"
        );
        assert_eq!(
            parse_version(b"ocserv 1.4.1\n")
                .expect("1.4 fixture")
                .version,
            "1.4.1"
        );
        assert!(parse_version(b"ocserv development\n").is_err());
        assert!(parse_version(&[0xff]).is_err());
    }

    #[tokio::test]
    async fn reads_packaged_version_banner_from_stderr() {
        let version_program = executable(
            "ocserv-version",
            "printf 'OpenConnect VPN Server 1.3.0\\n' >&2",
        );
        let fixture_directory = version_program.parent().expect("fixture parent").to_owned();
        let resources = FixedResources::new(
            PathBuf::from("/bin/false"),
            version_program,
            PathBuf::from("/bin/false"),
            PathBuf::from("/etc/hosts"),
            PathBuf::from("/proc/sys/kernel/random/boot_id"),
        )
        .expect("fixed resources");
        let version = Adapter::new(resources, Limits::default())
            .ocserv_version()
            .await
            .expect("stderr version banner");
        assert_eq!(version.version, "1.3.0");
        std::fs::remove_dir_all(fixture_directory).expect("remove version fixture");
    }

    #[test]
    fn parses_session_and_ban_fixtures() {
        for (users, bans) in [
            (
                include_bytes!("../fixtures/ocserv-1.2/users.json").as_slice(),
                include_bytes!("../fixtures/ocserv-1.2/bans.json").as_slice(),
            ),
            (
                include_bytes!("../fixtures/ocserv-1.3/users.json").as_slice(),
                include_bytes!("../fixtures/ocserv-1.3/bans.json").as_slice(),
            ),
            (
                include_bytes!("../fixtures/ocserv-1.4/users.json").as_slice(),
                include_bytes!("../fixtures/ocserv-1.4/bans.json").as_slice(),
            ),
        ] {
            let sessions = parse_sessions(users).expect("sessions fixture");
            assert_eq!(sessions.sessions.len(), 2);
            assert_eq!(sessions.sessions[1].remote_ip, "2001:db8::1");
            let bans = parse_ip_bans(bans).expect("bans fixture");
            assert_eq!(bans.bans.len(), 2);
            assert_eq!(bans.bans[1].seconds_remaining, None);
        }
        assert!(parse_ip_bans(b"[{\"IP\":\"192.0.2.1\"},]").is_err());
    }

    #[test]
    fn user_and_group_parsers_drop_hashes_and_reject_unsafe_names() {
        let users =
            parse_user_file(b"alice:*:$6$secret-hash\nbob:x:!$6$disabled\n").expect("users");
        assert_eq!(users.users.len(), 2);
        assert!(users.users[0].enabled);
        assert!(!users.users[1].enabled);
        assert!(!format!("{users:?}").contains("secret-hash"));
        let groups =
            parse_groups_from_user_file(b"alice:staff:$6$alice\nbob:admins,staff:$6$bob\n")
                .expect("groups");
        let staff = groups
            .groups
            .iter()
            .find(|group| group.group_name == "staff")
            .expect("staff");
        assert_eq!(staff.members, ["alice", "bob"]);
        assert!(parse_user_file(b"../root:hash\n").is_err());
        assert!(parse_groups_from_user_file(b"alice:staff;id:$6$hash\n").is_err());
    }

    #[tokio::test]
    async fn group_apply_is_atomic_when_replacement_fails() {
        let directory = std::env::temp_dir().join(format!("ocservia-group-{}", Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("directory");
        let users = directory.join("ocpasswd");
        std::fs::write(&users, b"alice:staff:$6$alice-hash\nbob:*:!$6$bob-hash\n")
            .expect("initial users");
        let resources = FixedResources::default()
            .with_user_resources(
                PathBuf::from("/bin/false"),
                users.clone(),
                PathBuf::from("/bin/false"),
                directory.join("key.pem"),
                String::from("test-key"),
            )
            .expect("resources");
        let adapter = Adapter::new(resources, Limits::default());
        assert!(
            adapter
                .group_apply("staff", &["alice".to_owned(), "bob".to_owned()], 1)
                .await
                .is_ok()
        );
        assert_eq!(
            adapter.group_list().await.expect("groups").groups[0].members,
            ["alice", "bob"]
        );
        assert!(
            adapter
                .desired_effect_observe("group_apply", "staff", 1)
                .await
                .expect("group marker")
                .applied
        );
        assert!(
            !adapter
                .desired_effect_observe("group_apply", "staff", 2)
                .await
                .expect("stale group marker")
                .applied
        );
        let updated = std::fs::read(&users).expect("updated");
        assert!(updated.starts_with(b"alice:staff:$6$alice-hash\nbob:staff:!$6$bob-hash\n"));
        assert!(adapter.group_apply("../staff", &[], 2).await.is_err());
        assert_eq!(std::fs::read(&users).expect("unchanged"), updated);
        std::fs::remove_dir_all(directory).expect("cleanup");
    }

    #[tokio::test]
    async fn failed_password_rotation_preserves_original_record() {
        let directory = std::env::temp_dir().join(format!("ocservia-secret-{}", Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("directory");
        let users = directory.join("ocpasswd");
        let original = b"alice:admins,staff:!$6$original-hash\n";
        std::fs::write(&users, original).expect("original");
        let openssl = executable("openssl-fixture", "printf rotated-password");
        let openssl_directory = openssl.parent().expect("openssl parent").to_owned();
        let ocpasswd = executable("ocpasswd-fixture", "printf corrupted > \"$2\"; exit 1");
        let ocpasswd_directory = ocpasswd.parent().expect("ocpasswd parent").to_owned();
        let resources = FixedResources::default()
            .with_user_resources(
                ocpasswd,
                users.clone(),
                openssl,
                directory.join("key.pem"),
                String::from("test-key"),
            )
            .expect("resources");
        let adapter = Adapter::new(resources, Limits::default());
        assert!(
            adapter
                .user_password_rotate("alice", "test-key", &[7_u8; 64], 2)
                .await
                .is_err()
        );
        assert_eq!(std::fs::read(&users).expect("preserved"), original);
        std::fs::remove_dir_all(directory).expect("cleanup directory");
        std::fs::remove_dir_all(openssl_directory).expect("cleanup openssl");
        std::fs::remove_dir_all(ocpasswd_directory).expect("cleanup ocpasswd");
    }

    #[tokio::test]
    async fn create_and_rotate_enforce_authoritative_existence_preconditions() {
        let directory = std::env::temp_dir().join(format!("ocservia-existence-{}", Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("directory");
        let users = directory.join("ocpasswd");
        let original = b"alice:admins,staff:!$6$original-hash\n";
        std::fs::write(&users, original).expect("original");
        let resources = FixedResources::default()
            .with_user_resources(
                PathBuf::from("/bin/false"),
                users.clone(),
                PathBuf::from("/bin/false"),
                directory.join("key.pem"),
                String::from("test-key"),
            )
            .expect("resources");
        let adapter = Adapter::new(resources, Limits::default());
        assert!(matches!(
            adapter
                .user_create("alice", "test-key", &[7_u8; 64], 1)
                .await,
            Err(AdapterError::InvalidRequest)
        ));
        assert_eq!(std::fs::read(&users).expect("create conflict"), original);
        assert!(matches!(
            adapter
                .user_password_rotate("bob", "test-key", &[7_u8; 64], 2)
                .await,
            Err(AdapterError::InvalidRequest)
        ));
        assert_eq!(std::fs::read(&users).expect("rotate missing"), original);
        std::fs::remove_dir_all(directory).expect("cleanup");
    }

    #[cfg(target_os = "linux")]
    async fn assert_effect_marker(
        adapter: &Adapter,
        mutation_kind: &str,
        resource_key: &str,
        revision: u64,
        expected: bool,
    ) {
        assert_eq!(
            adapter
                .desired_effect_observe(mutation_kind, resource_key, revision)
                .await
                .expect("observe effect marker")
                .applied,
            expected
        );
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    #[ignore = "requires native ocpasswd, OpenSSL, and an isolated test directory"]
    async fn native_user_and_group_operations() {
        let root = PathBuf::from(std::env::var("OCSERVIA_I13_NATIVE_ROOT").expect("native root"));
        let sealed = std::fs::read(root.join("sealed-password.bin")).expect("sealed password");
        let users = root.join("ocpasswd");
        let resources = FixedResources::default()
            .with_user_resources(
                PathBuf::from("/usr/bin/ocpasswd"),
                users.clone(),
                PathBuf::from("/usr/bin/openssl"),
                root.join("private.pem"),
                String::from("i13-native"),
            )
            .expect("fixed native resources");
        let adapter = Adapter::new(resources, Limits::default());
        adapter
            .user_create("alice", "i13-native", &sealed, 1)
            .await
            .expect("create native user");
        assert_effect_marker(&adapter, "user_create", "alice", 1, true).await;
        let after_create = std::fs::read(&users).expect("created record");
        assert!(
            adapter
                .user_password_rotate("bob", "i13-native", &sealed, 2)
                .await
                .is_err(),
            "rotate must not create a missing user"
        );
        assert_eq!(
            std::fs::read(&users).expect("unchanged create"),
            after_create
        );
        let listed = adapter.user_list().await.expect("list native users");
        assert_eq!(listed.users.len(), 1);
        assert_eq!(listed.users[0].username, "alice");
        assert!(listed.users[0].enabled);
        assert!(!format!("{listed:?}").contains("native-password-sentinel"));
        adapter
            .group_apply("staff", &["alice".to_owned()], 2)
            .await
            .expect("apply native group");
        let before_rotate = std::fs::read_to_string(&users).expect("before rotate");
        assert!(before_rotate.starts_with("alice:staff:"));
        adapter
            .user_disable("alice", 3)
            .await
            .expect("disable native user");
        assert!(!adapter.user_list().await.expect("list disabled").users[0].enabled);
        let locked_record = std::fs::read(&users).expect("locked record");
        assert!(
            adapter
                .user_create("alice", "i13-native", &sealed, 4)
                .await
                .is_err(),
            "create must not overwrite an existing user"
        );
        assert_eq!(
            std::fs::read(&users).expect("unchanged conflict"),
            locked_record,
            "create conflict changed password, group, or lock state"
        );
        adapter
            .user_password_rotate("alice", "i13-native", &sealed, 4)
            .await
            .expect("rotate disabled native user");
        assert_effect_marker(&adapter, "user_password_rotate", "alice", 4, true).await;
        assert!(
            !adapter
                .user_list()
                .await
                .expect("disabled after rotate")
                .users[0]
                .enabled
        );
        assert!(
            std::fs::read_to_string(&users)
                .expect("after rotate")
                .starts_with("alice:staff:!")
        );
        adapter
            .user_enable("alice", 5)
            .await
            .expect("enable native user");
        assert!(adapter.user_list().await.expect("enabled user").users[0].enabled);
        assert!(
            std::fs::read_to_string(&users)
                .expect("enabled record")
                .starts_with("alice:staff:")
        );
        assert_effect_marker(&adapter, "user_create", "alice", 1, true).await;
        assert_effect_marker(&adapter, "user_password_rotate", "alice", 3, false).await;
    }

    #[test]
    fn status_requires_exact_keys() {
        let status =
            parse_service_status(b"LoadState=loaded\nActiveState=active\nSubState=running\n")
                .expect("valid status");
        assert_eq!(status.active_state, "active");
        assert!(parse_service_status(b"ActiveState=active\n").is_err());
    }

    #[tokio::test]
    async fn child_timeout_and_output_limit_are_enforced() {
        let timeout_program = executable("slow-ocserv", "while :; do :; done");
        let timeout_directory = timeout_program.parent().expect("fixture parent").to_owned();
        let resources = FixedResources::new(
            PathBuf::from("/bin/false"),
            timeout_program,
            PathBuf::from("/bin/false"),
            PathBuf::from("/etc/hosts"),
            PathBuf::from("/proc/sys/kernel/random/boot_id"),
        )
        .expect("fixed resources");
        let adapter = Adapter::new(
            resources,
            Limits {
                timeout: Duration::from_millis(50),
                output_bytes: 1024,
            },
        );
        assert!(matches!(
            adapter.ocserv_version().await,
            Err(AdapterError::DeadlineExceeded)
        ));
        std::fs::remove_dir_all(timeout_directory).expect("remove timeout fixture");

        let noisy_program = executable(
            "noisy-ocserv",
            "dd if=/dev/zero bs=4096 count=1 2>/dev/null",
        );
        let noisy_directory = noisy_program.parent().expect("fixture parent").to_owned();
        let resources = FixedResources::new(
            PathBuf::from("/bin/false"),
            noisy_program,
            PathBuf::from("/bin/false"),
            PathBuf::from("/etc/hosts"),
            PathBuf::from("/proc/sys/kernel/random/boot_id"),
        )
        .expect("fixed resources");
        let adapter = Adapter::new(
            resources,
            Limits {
                timeout: Duration::from_secs(5),
                output_bytes: 1024,
            },
        );
        assert!(matches!(
            adapter.ocserv_version().await,
            Err(AdapterError::OutputLimit)
        ));
        std::fs::remove_dir_all(noisy_directory).expect("remove noisy fixture");
    }

    #[tokio::test]
    async fn mutations_use_only_fixed_programs_and_arguments() {
        let occtl = executable(
            "occtl",
            "case \"$*\" in 'disconnect id 42'|'terminate id 42'|'unban ip 192.0.2.9') exit 0;; *) exit 9;; esac",
        );
        let directory = occtl.parent().expect("fixture parent").to_owned();
        let systemctl = directory.join("systemctl");
        std::fs::write(
            &systemctl,
            "#!/bin/sh\n[ \"$*\" = 'reload ocserv.service --no-ask-password' ]\n",
        )
        .expect("write systemctl fixture");
        std::fs::set_permissions(&systemctl, std::fs::Permissions::from_mode(0o700))
            .expect("make systemctl executable");
        let boot = directory.join("boot_id");
        let boot_id = Uuid::now_v7().to_string();
        std::fs::write(&boot, &boot_id).expect("write boot fixture");
        let resources = FixedResources::new(
            systemctl,
            PathBuf::from("/bin/false"),
            occtl,
            PathBuf::from("/etc/hosts"),
            boot,
        )
        .expect("fixed resources");
        let adapter = Adapter::new(resources, Limits::default());
        assert!(adapter.session_disconnect("42", &boot_id).await.is_ok());
        assert!(adapter.session_terminate("42", &boot_id).await.is_ok());
        assert!(adapter.ip_ban_remove("192.0.2.9").await.is_ok());
        assert!(adapter.service_reload().await.is_ok());
        assert!(matches!(
            adapter.session_disconnect("042", &boot_id).await,
            Err(AdapterError::InvalidRequest)
        ));
        assert!(matches!(
            adapter
                .session_disconnect("42", &Uuid::now_v7().to_string())
                .await,
            Err(AdapterError::StaleBoot)
        ));
        std::fs::remove_dir_all(directory).expect("remove mutation fixture");
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    #[ignore = "requires an explicitly prepared native Ocserv test service"]
    async fn native_controlled_operations() {
        let disconnect_id = std::env::var("OCSERVIA_DISCONNECT_SESSION_ID")
            .expect("disconnect session ID is required");
        let terminate_id = std::env::var("OCSERVIA_TERMINATE_SESSION_ID")
            .expect("terminate session ID is required");
        let banned_ip = std::env::var("OCSERVIA_BANNED_IP").expect("banned IP is required");
        let boot_id =
            std::fs::read_to_string("/proc/sys/kernel/random/boot_id").expect("read boot ID");
        let adapter = Adapter::new(FixedResources::default(), Limits::default());

        adapter
            .session_disconnect(&disconnect_id, boot_id.trim())
            .await
            .expect("disconnect observed session");
        adapter
            .session_terminate(&terminate_id, boot_id.trim())
            .await
            .expect("terminate observed session");
        adapter
            .ip_ban_remove(&banned_ip)
            .await
            .expect("remove observed IP ban");
        adapter
            .service_reload()
            .await
            .expect("reload ocserv.service");
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    #[ignore = "requires an explicitly stopped native Ocserv test service"]
    async fn native_reload_failure_is_bounded() {
        let adapter = Adapter::new(FixedResources::default(), Limits::default());
        assert!(matches!(
            adapter.service_reload().await,
            Err(AdapterError::CommandFailed { .. })
        ));
    }
}
