//! Durable, root-owned self-upgrade boundary shared by privd and the
//! standalone `ocservia-upgrader` runner.
//!
//! privd never executes an upgrade itself. After independently verifying a
//! Controller-signed `AgentUpgrade` command it commits an immutable intent
//! below a fixed root-only hierarchy and starts the fixed
//! `ocservia-upgrader@<operation>.service` unit. The standalone runner then
//! resolves the trusted package, reuses the existing package verifier and
//! lifecycle scripts, and persists the durable local result that survives the
//! Agent/privd restart performed by the upgrade.
//!
//! All state lives under `/var/lib/ocservia-upgrade/operations/<uuid>/` as
//! `intent` (immutable after commit), `state`, and `result` evidence files.
//! The evidence is local recovery material only; the Control Plane operations
//! table remains authoritative.

#![forbid(unsafe_code)]

use std::fmt::Write as _;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write as _};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Component, Path, PathBuf};
use std::process::Command;
use std::time::{SystemTime, UNIX_EPOCH};

use rustix::fs::FlockOperation;
use sha2::{Digest as _, Sha256};
use uuid::Uuid;

/// Fixed schema version of the durable intent and result records.
pub const RECORD_SCHEMA_VERSION: u32 = 1;
/// Fixed root-only durable operations hierarchy.
pub const DEFAULT_OPERATIONS_DIR: &str = "/var/lib/ocservia-upgrade/operations";
/// Fixed operator-provisioned trusted package spool.
pub const DEFAULT_SPOOL_DIR: &str = "/var/lib/ocservia-upgrade/package-spool";
/// Fixed operator-provisioned release signing public key.
pub const DEFAULT_RELEASE_PUBLIC_KEY: &str = "/etc/ocservia/release-signing.pub.pem";
/// Fixed pinned DER SHA-256 fingerprint of the release signing key.
pub const DEFAULT_TRUSTED_FINGERPRINT: &str = "/etc/ocservia/trusted-release-key.sha256";
/// Fixed installed copy of the package verifier lifecycle script.
pub const DEFAULT_VERIFIER: &str = "/usr/libexec/ocservia/ocservia-agent-verify";
/// Fixed systemd template unit started per durable upgrade operation.
pub const UPGRADE_UNIT_TEMPLATE: &str = "ocservia-upgrader@{operation}.service";

const MAX_DETAIL_BYTES: usize = 160;
const MAX_SPOOL_PACKAGE_BYTES: u64 = 2 * 1024 * 1024 * 1024;
const SCHEDULE_LOCK_FILE: &str = ".schedule.lock";
const EXECUTION_LOCK_FILE: &str = ".run.lock";

/// The immutable release identity and command binding of one scheduled
/// upgrade. Rendering is fully derived from the Controller-signed command, so
/// byte-equality is the replay test for a duplicate delivery.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct UpgradeIntent {
    pub operation_id: Uuid,
    pub command_id: Uuid,
    pub target_version: String,
    pub package_sha256: [u8; 32],
    pub architecture: String,
    pub semantic_payload_sha256: [u8; 32],
}

impl UpgradeIntent {
    /// Validates and binds one immutable upgrade identity.
    ///
    /// # Errors
    ///
    /// Returns [`UpgradeStoreError::Invalid`] when any field is outside its
    /// canonical typed form.
    pub fn new(
        operation_id: [u8; 16],
        command_id: [u8; 16],
        target_version: &str,
        package_sha256: [u8; 32],
        architecture: &str,
        semantic_payload_sha256: [u8; 32],
    ) -> Result<Self, UpgradeStoreError> {
        require_uuid_v7(operation_id, "operation identity invalid")?;
        require_uuid_v7(command_id, "command identity invalid")?;
        if !ocservia_contracts::agent_upgrade::valid_target_version(target_version) {
            return Err(UpgradeStoreError::Invalid("target version invalid"));
        }
        if !ocservia_contracts::agent_upgrade::valid_architecture(architecture) {
            return Err(UpgradeStoreError::Invalid("architecture invalid"));
        }
        Ok(Self {
            operation_id: Uuid::from_bytes(operation_id),
            command_id: Uuid::from_bytes(command_id),
            target_version: target_version.to_owned(),
            package_sha256,
            architecture: architecture.to_owned(),
            semantic_payload_sha256,
        })
    }

    fn render(&self) -> String {
        let mut record = String::new();
        let _ = writeln!(record, "schema={RECORD_SCHEMA_VERSION}");
        let _ = writeln!(record, "operation_id={}", self.operation_id);
        let _ = writeln!(record, "command_id={}", self.command_id);
        let _ = writeln!(record, "target_version={}", self.target_version);
        let _ = writeln!(
            record,
            "package_sha256={}",
            hex::encode(self.package_sha256)
        );
        let _ = writeln!(record, "architecture={}", self.architecture);
        let _ = writeln!(
            record,
            "semantic_payload_sha256={}",
            hex::encode(self.semantic_payload_sha256)
        );
        record
    }

    fn parse(bytes: &[u8]) -> Result<Self, UpgradeStoreError> {
        let lines = decode_record_lines(bytes, 7)?;
        expect_line(&lines[0], "schema", &RECORD_SCHEMA_VERSION.to_string())?;
        let operation_id = parse_line_uuid(&lines[1], "operation_id")?;
        let command_id = parse_line_uuid(&lines[2], "command_id")?;
        let target_version = parse_line_value(&lines[3], "target_version")?;
        if !ocservia_contracts::agent_upgrade::valid_target_version(&target_version) {
            return Err(UpgradeStoreError::Unsafe("durable intent version invalid"));
        }
        let package_sha256 = parse_line_digest(&lines[4], "package_sha256")?;
        let architecture = parse_line_value(&lines[5], "architecture")?;
        if !ocservia_contracts::agent_upgrade::valid_architecture(&architecture) {
            return Err(UpgradeStoreError::Unsafe(
                "durable intent architecture invalid",
            ));
        }
        let semantic_payload_sha256 = parse_line_digest(&lines[6], "semantic_payload_sha256")?;
        Ok(Self {
            operation_id,
            command_id,
            target_version,
            package_sha256,
            architecture,
            semantic_payload_sha256,
        })
    }
}

/// Local durable state of one upgrade operation. These states are recovery
/// evidence, not a replacement for the Control Plane operations table.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum OperationState {
    /// The immutable intent is committed; the runner has not started.
    Accepted,
    /// The runner owns the operation and is executing the package lifecycle.
    Running,
    /// The verified package lifecycle completed.
    Succeeded,
    /// The runner refused or the package lifecycle failed.
    Failed,
    /// An operator rollback superseded the operation.
    RolledBack,
}

impl OperationState {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Accepted => "accepted",
            Self::Running => "running",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::RolledBack => "rolled_back",
        }
    }

    #[must_use]
    pub fn is_terminal(self) -> bool {
        matches!(self, Self::Succeeded | Self::Failed | Self::RolledBack)
    }

    fn parse(bytes: &[u8]) -> Result<Self, UpgradeStoreError> {
        let token = String::from_utf8_lossy(bytes);
        let token = token.trim_end_matches('\n');
        match token {
            "accepted" => Ok(Self::Accepted),
            "running" => Ok(Self::Running),
            "succeeded" => Ok(Self::Succeeded),
            "failed" => Ok(Self::Failed),
            "rolled_back" => Ok(Self::RolledBack),
            _ => Err(UpgradeStoreError::Unsafe(
                "durable operation state malformed",
            )),
        }
    }
}

/// Failures of the durable upgrade boundary.
#[derive(Debug)]
pub enum UpgradeStoreError {
    /// The typed intent or operation identity is malformed.
    Invalid(&'static str),
    /// An existing durable operation conflicts with this identity.
    IdentityConflict,
    /// Another durable upgrade operation is active on this node.
    ActiveConflict,
    /// The durable hierarchy failed a filesystem safety check.
    Unsafe(&'static str),
    /// The authorized package could not be resolved or verified.
    Package(String),
    /// The package lifecycle failed after the intent was committed.
    Lifecycle(String),
    /// Local I/O failure.
    Io(io::Error),
}

impl From<io::Error> for UpgradeStoreError {
    fn from(error: io::Error) -> Self {
        Self::Io(error)
    }
}

impl std::fmt::Display for UpgradeStoreError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Invalid(detail) => write!(formatter, "upgrade intent invalid: {detail}"),
            Self::IdentityConflict => {
                write!(
                    formatter,
                    "operation identity conflicts with a durable intent"
                )
            }
            Self::ActiveConflict => {
                write!(
                    formatter,
                    "another upgrade operation is active on this node"
                )
            }
            Self::Unsafe(detail) => write!(formatter, "durable upgrade state unsafe: {detail}"),
            Self::Package(detail) => write!(formatter, "trusted package refused: {detail}"),
            Self::Lifecycle(detail) => write!(formatter, "package lifecycle failed: {detail}"),
            Self::Io(error) => write!(formatter, "durable upgrade state I/O failed: {error}"),
        }
    }
}

impl std::error::Error for UpgradeStoreError {}

/// How privd starts the fixed upgrader unit for a committed intent.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum UpgradeTrigger {
    /// Start `ocservia-upgrader@<operation>.service` through the service
    /// manager. This is the only production path.
    Systemd,
    /// Do not start anything; used by tests that exercise only the durable
    /// commit boundary.
    Disabled,
}

/// Outcome of scheduling one Controller-authorized upgrade intent.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum ScheduleOutcome {
    /// The intent was committed durably and the fixed unit was started.
    Scheduled,
    /// An identical durable intent already existed and the fixed unit was
    /// started again; the runner replays idempotently.
    AlreadyScheduled,
}

/// privd-facing durable scheduling boundary: commit-first, then a fixed
/// best-effort unit start. Never executes any package lifecycle itself.
#[derive(Clone, Debug)]
pub struct UpgradeScheduler {
    operations_dir: PathBuf,
    trigger: UpgradeTrigger,
}

impl UpgradeScheduler {
    #[must_use]
    pub fn new(operations_dir: PathBuf, trigger: UpgradeTrigger) -> Self {
        Self {
            operations_dir,
            trigger,
        }
    }

    /// A scheduler that never persists or triggers; for tests of unrelated
    /// privd request families only.
    #[must_use]
    pub fn disabled() -> Self {
        Self::new(PathBuf::new(), UpgradeTrigger::Disabled)
    }

    /// The durable operations root backing this scheduler.
    #[must_use]
    pub fn operations_root(&self) -> &Path {
        &self.operations_dir
    }

    /// Commits the immutable intent (or replays an identical one) and starts
    /// the fixed upgrader unit.
    ///
    /// # Errors
    ///
    /// Rejects identity conflicts, concurrent active operations, and unsafe
    /// durable state before anything is triggered.
    pub fn schedule_and_trigger(
        &self,
        intent: &UpgradeIntent,
    ) -> Result<ScheduleOutcome, UpgradeStoreError> {
        ensure_operations_root(&self.operations_dir)?;
        let operation_dir = self.operations_dir.join(intent.operation_id.to_string());
        let intent_bytes = intent.render().into_bytes();
        let outcome = {
            let _schedule = UpgradeLock::acquire_schedule(&self.operations_dir)?;
            cleanup_stale_temporaries(&operation_dir, TempOwner::Schedule)?;
            match read_operation_pieces(&operation_dir)? {
                OperationPieces::Absent => {
                    if self.active_sibling(Some(intent.operation_id))?.is_some() {
                        return Err(UpgradeStoreError::ActiveConflict);
                    }
                    create_operation_dir(&operation_dir)?;
                    commit_intent(&operation_dir, &intent_bytes)?;
                    ScheduleOutcome::Scheduled
                }
                OperationPieces::Partial { intent: None } => {
                    if self.active_sibling(Some(intent.operation_id))?.is_some() {
                        return Err(UpgradeStoreError::ActiveConflict);
                    }
                    write_atomic(
                        &operation_dir.join("intent"),
                        &intent_bytes,
                        TempOwner::Schedule,
                    )?;
                    write_atomic(
                        &operation_dir.join("state"),
                        format!("{}\n", OperationState::Accepted.as_str()).as_bytes(),
                        TempOwner::Schedule,
                    )?;
                    ScheduleOutcome::Scheduled
                }
                OperationPieces::Partial {
                    intent: Some(existing),
                } => {
                    if existing != *intent {
                        return Err(UpgradeStoreError::IdentityConflict);
                    }
                    write_atomic(
                        &operation_dir.join("state"),
                        format!("{}\n", OperationState::Accepted.as_str()).as_bytes(),
                        TempOwner::Schedule,
                    )?;
                    ScheduleOutcome::Scheduled
                }
                OperationPieces::Complete { intent: existing } => {
                    if existing != *intent {
                        return Err(UpgradeStoreError::IdentityConflict);
                    }
                    ScheduleOutcome::AlreadyScheduled
                }
            }
        };
        self.trigger(intent.operation_id)?;
        Ok(outcome)
    }

    fn trigger(&self, operation_id: Uuid) -> Result<(), UpgradeStoreError> {
        match self.trigger {
            UpgradeTrigger::Disabled => Ok(()),
            UpgradeTrigger::Systemd => {
                let unit = UPGRADE_UNIT_TEMPLATE.replace("{operation}", &operation_id.to_string());
                let status = Command::new("/usr/bin/systemctl")
                    .arg("start")
                    .arg(&unit)
                    .status()
                    .map_err(UpgradeStoreError::Io)?;
                if status.success() {
                    Ok(())
                } else {
                    Err(UpgradeStoreError::Io(io::Error::other(format!(
                        "systemctl start {unit} failed"
                    ))))
                }
            }
        }
    }

    fn active_sibling(&self, exclude: Option<Uuid>) -> Result<Option<Uuid>, UpgradeStoreError> {
        for entry in fs::read_dir(&self.operations_dir)? {
            let entry = entry.map_err(UpgradeStoreError::Io)?;
            let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
                return Err(UpgradeStoreError::Unsafe(
                    "durable operations hierarchy has a non-UTF-8 entry",
                ));
            };
            let Ok(candidate) = Uuid::parse_str(&name) else {
                continue;
            };
            if name != candidate.to_string() || candidate.get_version_num() != 7 {
                continue;
            }
            if Some(candidate) == exclude {
                continue;
            }
            let state = load_state(&entry.path())?;
            if state == OperationState::Accepted || state == OperationState::Running {
                return Ok(Some(candidate));
            }
        }
        Ok(None)
    }
}

enum OperationPieces {
    Absent,
    /// The operation directory exists but no state was committed yet. A
    /// missing `intent` marks a crash between directory creation and the
    /// intent commit; the identical retry completes it.
    Partial {
        intent: Option<UpgradeIntent>,
    },
    Complete {
        intent: UpgradeIntent,
    },
}

fn commit_intent(operation_dir: &Path, intent_bytes: &[u8]) -> Result<(), UpgradeStoreError> {
    write_atomic(
        &operation_dir.join("intent"),
        intent_bytes,
        TempOwner::Schedule,
    )?;
    write_atomic(
        &operation_dir.join("state"),
        format!("{}\n", OperationState::Accepted.as_str()).as_bytes(),
        TempOwner::Schedule,
    )?;
    Ok(())
}

fn read_operation_pieces(operation_dir: &Path) -> Result<OperationPieces, UpgradeStoreError> {
    match fs::symlink_metadata(operation_dir) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(OperationPieces::Absent);
        }
        Err(error) => return Err(UpgradeStoreError::Io(error)),
        Ok(_) => {}
    }
    validate_directory_strict(operation_dir)?;
    let intent_path = operation_dir.join("intent");
    let state_path = operation_dir.join("state");
    let intent_present = fs::symlink_metadata(&intent_path).is_ok();
    let state_present = fs::symlink_metadata(&state_path).is_ok();
    match (intent_present, state_present) {
        (false, false) => Ok(OperationPieces::Partial { intent: None }),
        (true, false) => Ok(OperationPieces::Partial {
            intent: Some(load_intent(operation_dir)?),
        }),
        (true, true) => {
            // The state file is loaded purely for validation; any durable
            // state combined with a matching intent is a legal replay.
            load_state(operation_dir)?;
            Ok(OperationPieces::Complete {
                intent: load_intent(operation_dir)?,
            })
        }
        (false, true) => Err(UpgradeStoreError::Unsafe(
            "durable operation state exists without its immutable intent",
        )),
    }
}

fn load_intent(operation_dir: &Path) -> Result<UpgradeIntent, UpgradeStoreError> {
    let path = operation_dir.join("intent");
    validate_record_file(&path)?;
    let bytes = fs::read(&path).map_err(UpgradeStoreError::Io)?;
    if bytes.len() > 1024 {
        return Err(UpgradeStoreError::Unsafe(
            "durable intent exceeds its bound",
        ));
    }
    UpgradeIntent::parse(&bytes)
}

fn load_state(operation_dir: &Path) -> Result<OperationState, UpgradeStoreError> {
    let path = operation_dir.join("state");
    validate_record_file(&path)?;
    let mut bytes = Vec::new();
    File::open(&path)
        .map_err(UpgradeStoreError::Io)?
        .take(64)
        .read_to_end(&mut bytes)
        .map_err(UpgradeStoreError::Io)?;
    OperationState::parse(&bytes)
}

/// One durable terminal upgrade outcome read back from the fixed store.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct DurableUpgradeResult {
    pub operation_id: Uuid,
    pub state: OperationState,
    pub target_version: String,
    pub package_sha256: [u8; 32],
    pub completed_unix: u64,
    pub detail: String,
    pub result_sha256: [u8; 32],
}

/// Reads the bounded most-recent terminal upgrade outcomes from the fixed
/// root-owned store. This is the only public read surface of the durable
/// evidence: callers cannot select operation IDs, paths, or content, and
/// every present record must parse fail-closed.
///
/// # Errors
///
/// Returns [`UpgradeStoreError`] when the hierarchy or any present result
/// record fails its fail-closed validation.
pub fn read_recent_results(
    operations_dir: &Path,
    limit: usize,
) -> Result<Vec<DurableUpgradeResult>, UpgradeStoreError> {
    let limit = limit.clamp(1, 32);
    let mut results = Vec::new();
    let entries = match fs::read_dir(operations_dir) {
        Ok(entries) => entries,
        // A node that never ran an upgrade has no durable evidence at all.
        Err(failure) if failure.kind() == std::io::ErrorKind::NotFound => return Ok(results),
        Err(failure) => return Err(UpgradeStoreError::Io(failure)),
    };
    for entry in entries {
        let entry = entry.map_err(UpgradeStoreError::Io)?;
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            return Err(UpgradeStoreError::Unsafe(
                "durable operations hierarchy has a non-UTF-8 entry",
            ));
        };
        let Ok(candidate) = Uuid::parse_str(&name) else {
            continue;
        };
        if name != candidate.to_string() || candidate.get_version_num() != 7 {
            continue;
        }
        let record_dir = entry.path();
        validate_directory_strict(&record_dir)?;
        let intent = load_intent(&record_dir)?;
        if intent.operation_id != candidate {
            return Err(UpgradeStoreError::Unsafe(
                "durable intent operation identity does not match its directory",
            ));
        }
        let result_path = entry.path().join("result");
        match fs::symlink_metadata(&result_path) {
            Err(failure) if failure.kind() == io::ErrorKind::NotFound => {
                // No terminal result record exists yet; the operation is still
                // in flight and simply carries no reconciliation evidence.
                continue;
            }
            Err(failure) => return Err(UpgradeStoreError::Io(failure)),
            Ok(_) => {}
        }
        validate_record_file(&result_path)?;
        let bytes = fs::read(&result_path).map_err(UpgradeStoreError::Io)?;
        if bytes.len() > 1024 {
            return Err(UpgradeStoreError::Unsafe(
                "durable result exceeds its bound",
            ));
        }
        let lines = decode_record_lines(&bytes, 7)?;
        expect_line(&lines[0], "schema", &RECORD_SCHEMA_VERSION.to_string())?;
        let state = OperationState::parse(parse_line_value(&lines[1], "state")?.as_bytes())?;
        let operation_id = parse_line_uuid(&lines[2], "operation_id")?;
        if operation_id != candidate {
            return Err(UpgradeStoreError::Unsafe(
                "durable result operation identity does not match its directory",
            ));
        }
        let target_version = parse_line_value(&lines[3], "target_version")?;
        let package_sha256 = parse_line_digest(&lines[4], "package_sha256")?;
        if target_version != intent.target_version || package_sha256 != intent.package_sha256 {
            return Err(UpgradeStoreError::Unsafe(
                "durable result release identity does not match its intent",
            ));
        }
        let completed_unix = parse_line_value(&lines[5], "completed_unix")?
            .parse::<u64>()
            .map_err(|_| UpgradeStoreError::Unsafe("durable result completion time malformed"))?;
        let detail = parse_line_value(&lines[6], "detail")?;
        if !state.is_terminal() {
            return Err(UpgradeStoreError::Unsafe(
                "durable result records a non-terminal state",
            ));
        }
        if completed_unix == 0 || completed_unix > i64::MAX as u64 {
            return Err(UpgradeStoreError::Unsafe(
                "durable result completion time malformed",
            ));
        }
        let result_sha256 = Sha256::digest(&bytes).into();
        results.push(DurableUpgradeResult {
            operation_id,
            state,
            target_version,
            package_sha256,
            completed_unix,
            detail,
            result_sha256,
        });
    }
    // UUIDv7 ordering is creation-time ordering, so the newest outcomes win
    // the bounded report window deterministically.
    results.sort_by(|left, right| {
        right
            .operation_id
            .as_u128()
            .cmp(&left.operation_id.as_u128())
    });
    results.truncate(limit);
    Ok(results)
}

/// The standalone runner bound to one filesystem root. Production always uses
/// `/`; a different root exists only for staged lifecycle verification.
#[derive(Clone, Debug)]
pub struct UpgradeRunner {
    root: PathBuf,
}

impl UpgradeRunner {
    #[must_use]
    pub fn new(root: PathBuf) -> Self {
        Self { root }
    }

    #[must_use]
    pub fn is_real_host(&self) -> bool {
        self.root == Path::new("/")
    }

    #[must_use]
    pub fn operations_dir(&self) -> PathBuf {
        self.root.join("var/lib/ocservia-upgrade/operations")
    }

    #[must_use]
    pub fn spool_dir(&self) -> PathBuf {
        self.root.join(DEFAULT_SPOOL_DIR.trim_start_matches('/'))
    }

    #[must_use]
    pub fn release_public_key(&self) -> PathBuf {
        self.root
            .join(DEFAULT_RELEASE_PUBLIC_KEY.trim_start_matches('/'))
    }

    #[must_use]
    pub fn trusted_fingerprint(&self) -> PathBuf {
        self.root
            .join(DEFAULT_TRUSTED_FINGERPRINT.trim_start_matches('/'))
    }

    #[must_use]
    pub fn verifier(&self) -> PathBuf {
        self.root.join(DEFAULT_VERIFIER.trim_start_matches('/'))
    }

    fn installed_binary(&self, name: &str) -> PathBuf {
        self.root.join(format!("usr/libexec/ocservia/{name}"))
    }

    /// The durable, package-digest-bound record the installer writes only
    /// after every binary, verifier, and unit of one package is fully
    /// installed and synced. Crash convergence trusts this record alone.
    fn installation_commit(&self) -> PathBuf {
        self.root.join("var/lib/ocservia-upgrade/installed-commit")
    }

    /// Executes (or replays) exactly one durable upgrade operation.
    ///
    /// # Errors
    ///
    /// Every refusal is persisted as a durable `failed` result before the
    /// error is returned, except refusals of the operation identity itself.
    pub fn run(&self, operation: &str) -> Result<OperationState, UpgradeStoreError> {
        let operation_id = parse_canonical_operation(operation)?;
        let operations_root = self.operations_dir();
        validate_directory_strict(&operations_root)?;
        let operation_dir = operations_root.join(operation_id.to_string());
        let intent = load_intent(&operation_dir)?;
        let state = load_state(&operation_dir)?;
        if state.is_terminal() {
            tracing::info!(operation = %operation_id, state = state.as_str(), "durable upgrade already terminal");
            return Ok(state);
        }
        let _execution = UpgradeLock::acquire_execution(&operations_root)?;
        cleanup_stale_temporaries(&operation_dir, TempOwner::Run)?;
        let state = load_state(&operation_dir)?;
        if state.is_terminal() {
            return Ok(state);
        }
        self.execute(&operation_dir, &intent)
    }

    #[allow(clippy::too_many_lines)]
    fn execute(
        &self,
        operation_dir: &Path,
        intent: &UpgradeIntent,
    ) -> Result<OperationState, UpgradeStoreError> {
        if ocservia_contracts::agent_upgrade::runtime_architecture()
            != Some(intent.architecture.as_str())
        {
            return refuse(
                operation_dir,
                intent,
                UpgradeStoreError::Package("intent architecture mismatches this host".to_owned()),
            );
        }
        let archive = self.spool_dir().join(format!(
            "ocservia-agent-{}-linux-{}.tar.gz",
            intent.target_version, intent.architecture
        ));
        let checksum = {
            let mut name = archive.as_os_str().to_os_string();
            name.push(".sha256");
            PathBuf::from(name)
        };
        let signature = {
            let mut name = checksum.as_os_str().to_os_string();
            name.push(".sig");
            PathBuf::from(name)
        };
        for input in [&archive, &checksum, &signature] {
            if let Err(failure) = validate_spool_file(input) {
                return refuse(operation_dir, intent, failure);
            }
        }
        let digest = sha256_file(&archive)?;
        if digest != intent.package_sha256 {
            return refuse(
                operation_dir,
                intent,
                UpgradeStoreError::Package(
                    "spool package digest does not match the authorized intent".to_owned(),
                ),
            );
        }
        let fingerprint = match load_fingerprint(&self.trusted_fingerprint()) {
            Ok(fingerprint) => fingerprint,
            Err(failure) => return refuse(operation_dir, intent, failure),
        };
        if let Err(failure) = validate_public_key(&self.release_public_key()) {
            return refuse(operation_dir, intent, failure);
        }
        let package_root = match self.verify_package(&archive, &checksum, &signature, &fingerprint)
        {
            Ok(root) => root,
            Err(failure) => {
                return refuse(
                    operation_dir,
                    intent,
                    UpgradeStoreError::Package(format!("package verification failed: {failure}")),
                );
            }
        };
        if let Err(failure) = validate_verified_marker(&package_root, &digest) {
            return refuse(operation_dir, intent, failure);
        }
        let agent_source = package_root.join("rust/target/release/ocservia-agent");
        let privd_source = package_root.join("rust/target/release/ocservia-privd");
        for source in [&agent_source, &privd_source] {
            if let Err(failure) = validate_regular_executable(source) {
                return refuse(operation_dir, intent, failure);
            }
        }
        let expected_agent = sha256_file(&agent_source)?;
        let expected_privd = sha256_file(&privd_source)?;
        let installed_agent = sha256_if_present(&self.installed_binary("ocservia-agent"))?;
        let installed_privd = sha256_if_present(&self.installed_binary("ocservia-privd"))?;
        let agent_matched = installed_agent.is_some_and(|digest| digest == expected_agent);
        let privd_matched = installed_privd.is_some_and(|digest| digest == expected_privd);
        let committed = match read_installation_commit(&self.installation_commit())? {
            Some(record) => record == hex::encode(digest),
            None => false,
        };
        // Crash convergence is proven exclusively by the digest-bound
        // installation commit record: matching agent and privd binaries
        // alone can be a partial install (for example both binaries were
        // replaced before the upgrader, verifier, or units were). Only a
        // complete, synced installation writes the record.
        let replaced = committed && agent_matched && privd_matched;
        if !replaced && (agent_matched || privd_matched) {
            // An interrupted lifecycle already modified this host. Re-running
            // the destructive backup step would snapshot the mixed state as
            // the previous release, and guessing success would certify it;
            // the rollback snapshot from the original attempt is preserved,
            // so refuse durably and leave recovery to the operator.
            return refuse(
                operation_dir,
                intent,
                UpgradeStoreError::Unsafe(
                    "installation was interrupted before its commit record; the rollback snapshot is preserved",
                ),
            );
        }
        // Execution-time downgrade fence: the authorization was an upgrade
        // when it was scheduled, but the host may have moved on since. The
        // currently running release identity (this runner ships with the
        // installed package) must still be strictly older than the target.
        // The already-replaced crash-recovery branch above is exempt: its
        // binaries match the authorized package, so the operation finished.
        if !replaced
            && !ocservia_contracts::agent_upgrade::is_strict_upgrade(
                ocservia_contracts::agent_upgrade::release_version(),
                &intent.target_version,
            )
        {
            return refuse(
                operation_dir,
                intent,
                UpgradeStoreError::Package(format!(
                    "target version {} is not newer than the running release {}",
                    intent.target_version,
                    ocservia_contracts::agent_upgrade::release_version()
                )),
            );
        }
        write_atomic(
            &operation_dir.join("state"),
            format!("{}\n", OperationState::Running.as_str()).as_bytes(),
            TempOwner::Run,
        )?;
        let outcome = if replaced {
            // Crash window: the replacement finished but the service restart
            // or result persistence did not. Never re-run the destructive
            // backup step over the matched snapshot of the previous release.
            if self.is_real_host()
                && let Err(failure) = restart_services()
            {
                return refuse(
                    operation_dir,
                    intent,
                    UpgradeStoreError::Lifecycle(format!("service restart failed: {failure}")),
                );
            }
            finish(operation_dir, intent, "verified package already installed")
        } else {
            if let Err(failure) = self.run_lifecycle(&package_root) {
                return refuse(operation_dir, intent, UpgradeStoreError::Lifecycle(failure));
            }
            let installed_agent = sha256_if_present(&self.installed_binary("ocservia-agent"))?;
            let installed_privd = sha256_if_present(&self.installed_binary("ocservia-privd"))?;
            if installed_agent != Some(expected_agent) || installed_privd != Some(expected_privd) {
                return refuse(
                    operation_dir,
                    intent,
                    UpgradeStoreError::Lifecycle(
                        "lifecycle completed without installing the authorized binaries".to_owned(),
                    ),
                );
            }
            if !matches!(
                read_installation_commit(&self.installation_commit())?,
                Some(record) if record == hex::encode(digest)
            ) {
                return refuse(
                    operation_dir,
                    intent,
                    UpgradeStoreError::Lifecycle(
                        "lifecycle completed without recording the installation commit".to_owned(),
                    ),
                );
            }
            finish(
                operation_dir,
                intent,
                "verified package lifecycle completed",
            )
        };
        self.cleanup_staging(&package_root);
        outcome
    }

    fn run_lifecycle(&self, package_root: &Path) -> Result<(), String> {
        let script = package_root.join("scripts/upgrade-agent.sh");
        validate_regular_executable(&script).map_err(|failure| failure.to_string())?;
        let mut command = Command::new(&script);
        if self.is_real_host() {
            command.env_remove("DESTDIR");
        } else {
            command.env("DESTDIR", &self.root);
        }
        let output = command.output().map_err(|failure| failure.to_string())?;
        if output.status.success() {
            return Ok(());
        }
        Err(format!(
            "upgrade-agent.sh exited with {}: {}",
            output
                .status
                .code()
                .map_or_else(|| "a signal".to_owned(), |code| code.to_string()),
            bounded_detail(&String::from_utf8_lossy(&output.stderr),)
        ))
    }

    fn verify_package(
        &self,
        archive: &Path,
        checksum: &Path,
        signature: &Path,
        fingerprint: &str,
    ) -> Result<PathBuf, UpgradeStoreError> {
        let verifier = self.verifier();
        validate_regular_executable(&verifier)?;
        let staging_root = self.root.join("var/lib/ocservia-upgrade/package-staging");
        let mut command = Command::new(&verifier);
        command
            .arg(archive)
            .arg(checksum)
            .arg(signature)
            .arg(self.release_public_key())
            .env("AGENT_TRUSTED_KEY_SHA256", fingerprint);
        if self.is_real_host() {
            command.env_remove("DESTDIR");
        } else {
            command.env("DESTDIR", &self.root);
        }
        let output = command.output().map_err(UpgradeStoreError::Io)?;
        if !output.status.success() {
            return Err(UpgradeStoreError::Package(
                "verified package staging refused the spool inputs".to_owned(),
            ));
        }
        let printed = String::from_utf8_lossy(&output.stdout).trim().to_owned();
        let package_root = PathBuf::from(printed);
        let relative = package_root.strip_prefix(&staging_root).map_err(|_| {
            UpgradeStoreError::Package("verifier output escapes trusted staging".to_owned())
        })?;
        if relative.components().count() != 3
            || relative
                .components()
                .any(|component| !matches!(component, Component::Normal(_)))
        {
            return Err(UpgradeStoreError::Package(
                "verifier output is not a verified package root".to_owned(),
            ));
        }
        validate_directory_strict(&package_root)?;
        Ok(package_root)
    }

    /// Best-effort removal of the verifier's scratch staging directory once
    /// the operation reached a terminal outcome; evidence is persisted first.
    fn cleanup_staging(&self, package_root: &Path) {
        let Some(staging) = package_root.parent().and_then(Path::parent) else {
            return;
        };
        let staging_root = self.root.join("var/lib/ocservia-upgrade/package-staging");
        if staging.parent() == Some(staging_root.as_path())
            && let Err(error) = fs::remove_dir_all(staging)
        {
            tracing::warn!(error = %error, staging = %staging.display(), "verified staging cleanup failed");
        }
    }
}

fn write_terminal(
    operation_dir: &Path,
    intent: &UpgradeIntent,
    state: OperationState,
    detail: &str,
) -> Result<(), UpgradeStoreError> {
    let completed = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| UpgradeStoreError::Unsafe("system clock unavailable"))?;
    let mut record = String::new();
    let _ = writeln!(record, "schema={RECORD_SCHEMA_VERSION}");
    let _ = writeln!(record, "state={}", state.as_str());
    let _ = writeln!(record, "operation_id={}", intent.operation_id);
    let _ = writeln!(record, "target_version={}", intent.target_version);
    let _ = writeln!(
        record,
        "package_sha256={}",
        hex::encode(intent.package_sha256)
    );
    let _ = writeln!(record, "completed_unix={}", completed.as_secs());
    let _ = writeln!(record, "detail={}", bounded_detail(detail));
    write_atomic(
        &operation_dir.join("result"),
        record.as_bytes(),
        TempOwner::Run,
    )?;
    write_atomic(
        &operation_dir.join("state"),
        format!("{}\n", state.as_str()).as_bytes(),
        TempOwner::Run,
    )?;
    Ok(())
}

fn finish(
    operation_dir: &Path,
    intent: &UpgradeIntent,
    detail: &str,
) -> Result<OperationState, UpgradeStoreError> {
    write_terminal(operation_dir, intent, OperationState::Succeeded, detail)?;
    Ok(OperationState::Succeeded)
}

/// Persists the durable failure evidence first, then surfaces the same
/// refusal to the caller so exit status and local evidence agree.
fn refuse(
    operation_dir: &Path,
    intent: &UpgradeIntent,
    failure: UpgradeStoreError,
) -> Result<OperationState, UpgradeStoreError> {
    if let Err(persisted) = write_terminal(
        operation_dir,
        intent,
        OperationState::Failed,
        &failure.to_string(),
    ) {
        tracing::warn!(error = %persisted, "durable failure result could not be persisted");
    }
    Err(failure)
}

struct UpgradeLock {
    _file: File,
}

impl UpgradeLock {
    fn open(operations_dir: &Path, name: &str) -> Result<File, UpgradeStoreError> {
        let path = operations_dir.join(name);
        let file = match OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&path)
        {
            Ok(file) => file,
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
                validate_record_file(&path)?;
                OpenOptions::new()
                    .write(true)
                    .open(&path)
                    .map_err(UpgradeStoreError::Io)?
            }
            Err(error) => return Err(UpgradeStoreError::Io(error)),
        };
        Ok(file)
    }

    /// The scheduling critical section covers only the durable read, active
    /// scan, and intent commit - never the unit trigger - so a concurrent
    /// scheduler for the same command waits briefly here and then observes
    /// the committed intent as an exact replay instead of being rejected.
    fn acquire_schedule(operations_dir: &Path) -> Result<Self, UpgradeStoreError> {
        let file = Self::open(operations_dir, SCHEDULE_LOCK_FILE)?;
        rustix::fs::flock(&file, FlockOperation::LockExclusive)
            .map_err(|failure| UpgradeStoreError::Io(io::Error::from(failure)))?;
        Ok(Self { _file: file })
    }

    /// The execution lock stays non-blocking: a second runner instance for
    /// one operation is genuinely conflicting, and systemd's own restart
    /// convergence owns recovery after a crashed runner.
    fn acquire_execution(operations_dir: &Path) -> Result<Self, UpgradeStoreError> {
        let file = Self::open(operations_dir, EXECUTION_LOCK_FILE)?;
        rustix::fs::flock(&file, FlockOperation::NonBlockingLockExclusive)
            .map_err(|_| UpgradeStoreError::ActiveConflict)?;
        Ok(Self { _file: file })
    }
}

fn restart_services() -> Result<(), String> {
    let status = Command::new("/usr/bin/systemctl")
        .args([
            "try-restart",
            "ocservia-privd.service",
            "ocservia-agent.service",
        ])
        .status()
        .map_err(|failure| failure.to_string())?;
    if status.success() {
        Ok(())
    } else {
        Err("systemctl try-restart failed".to_owned())
    }
}

fn parse_canonical_operation(value: &str) -> Result<Uuid, UpgradeStoreError> {
    if value.len() != 36
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() || byte == b'-')
    {
        return Err(UpgradeStoreError::Invalid("operation identity malformed"));
    }
    let parsed = Uuid::parse_str(value)
        .map_err(|_| UpgradeStoreError::Invalid("operation identity malformed"))?;
    if parsed.get_version_num() != 7 || parsed.to_string() != value {
        return Err(UpgradeStoreError::Invalid(
            "operation identity must be canonical UUIDv7",
        ));
    }
    Ok(parsed)
}

fn require_uuid_v7(value: [u8; 16], detail: &'static str) -> Result<(), UpgradeStoreError> {
    if Uuid::from_bytes(value).get_version_num() == 7 {
        Ok(())
    } else {
        Err(UpgradeStoreError::Invalid(detail))
    }
}

fn decode_record_lines(bytes: &[u8], expected: usize) -> Result<Vec<String>, UpgradeStoreError> {
    let text = std::str::from_utf8(bytes)
        .map_err(|_| UpgradeStoreError::Unsafe("durable record is not UTF-8"))?;
    let mut lines: Vec<String> = text.split('\n').map(str::to_owned).collect();
    if lines.last().is_some_and(String::is_empty) {
        lines.pop();
    }
    if lines.len() != expected {
        return Err(UpgradeStoreError::Unsafe(
            "durable record line count malformed",
        ));
    }
    Ok(lines)
}

fn parse_line_value(line: &str, key: &str) -> Result<String, UpgradeStoreError> {
    let value = line
        .strip_prefix(&format!("{key}="))
        .ok_or(UpgradeStoreError::Unsafe(
            "durable record key order malformed",
        ))?;
    if value.is_empty() || value.contains('\n') {
        return Err(UpgradeStoreError::Unsafe("durable record value malformed"));
    }
    Ok(value.to_owned())
}

fn expect_line(line: &str, key: &str, expected: &str) -> Result<(), UpgradeStoreError> {
    if parse_line_value(line, key)? == expected {
        Ok(())
    } else {
        Err(UpgradeStoreError::Unsafe("durable record schema mismatch"))
    }
}

fn parse_line_uuid(line: &str, key: &str) -> Result<Uuid, UpgradeStoreError> {
    let value = parse_line_value(line, key)?;
    let parsed = Uuid::parse_str(&value)
        .map_err(|_| UpgradeStoreError::Unsafe("durable record identity malformed"))?;
    if parsed.get_version_num() != 7 || parsed.to_string() != value {
        return Err(UpgradeStoreError::Unsafe(
            "durable record identity malformed",
        ));
    }
    Ok(parsed)
}

fn parse_line_digest(line: &str, key: &str) -> Result<[u8; 32], UpgradeStoreError> {
    let value = parse_line_value(line, key)?;
    if value.len() != 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(UpgradeStoreError::Unsafe("durable record digest malformed"));
    }
    let decoded = hex::decode(value)
        .map_err(|_| UpgradeStoreError::Unsafe("durable record digest malformed"))?;
    decoded
        .try_into()
        .map_err(|_| UpgradeStoreError::Unsafe("durable record digest malformed"))
}

fn bounded_detail(detail: &str) -> String {
    let mut bounded = String::new();
    for byte in detail.bytes().take(MAX_DETAIL_BYTES) {
        if (0x20..0x7f).contains(&byte) {
            bounded.push(byte as char);
        } else {
            bounded.push('.');
        }
    }
    bounded
}

fn trusted_euid() -> u32 {
    rustix::process::geteuid().as_raw()
}

fn validate_directory_strict(directory: &Path) -> Result<(), UpgradeStoreError> {
    let metadata = fs::symlink_metadata(directory).map_err(UpgradeStoreError::Io)?;
    if !metadata.is_dir()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.permissions().mode() & 0o022 != 0
    {
        return Err(UpgradeStoreError::Unsafe(
            "trusted upgrade path ancestry must be operator-owned real directories",
        ));
    }
    Ok(())
}

fn validate_record_file(path: &Path) -> Result<(), UpgradeStoreError> {
    let metadata = fs::symlink_metadata(path).map_err(UpgradeStoreError::Io)?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o777 != 0o600
    {
        return Err(UpgradeStoreError::Unsafe(
            "durable upgrade record must be an operator-owned one-link mode 0600 regular file",
        ));
    }
    Ok(())
}

fn validate_regular_executable(path: &Path) -> Result<(), UpgradeStoreError> {
    let metadata = fs::symlink_metadata(path).map_err(UpgradeStoreError::Io)?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o111 == 0
        || metadata.permissions().mode() & 0o022 != 0
    {
        return Err(UpgradeStoreError::Package(
            "trusted package executable has unsafe metadata".to_owned(),
        ));
    }
    Ok(())
}

/// Reads the installer's durable commit record. `Ok(None)` means no record
/// exists (a pre-upgrade host or a fresh operation); a present but
/// non-regular, symlinked, oversized, or malformed record is unsafe and
/// fails closed instead of being ignored, because convergence decisions
/// hinge on it.
fn read_installation_commit(path: &Path) -> Result<Option<String>, UpgradeStoreError> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(UpgradeStoreError::Io(error)),
    };
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() > 96 {
        return Err(UpgradeStoreError::Unsafe(
            "installation commit record is not a bounded regular file",
        ));
    }
    let record = fs::read_to_string(path).map_err(UpgradeStoreError::Io)?;
    let digest = record
        .strip_prefix("archive_sha256=")
        .and_then(|digest| digest.strip_suffix('\n'))
        .unwrap_or_default();
    if digest.len() != 64
        || !digest
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(UpgradeStoreError::Unsafe(
            "installation commit record is malformed",
        ));
    }
    Ok(Some(digest.to_owned()))
}

fn validate_spool_file(path: &Path) -> Result<(), UpgradeStoreError> {
    let metadata = fs::symlink_metadata(path).map_err(|error| {
        if error.kind() == io::ErrorKind::NotFound {
            UpgradeStoreError::Package(
                "trusted package spool does not provide the authorized release".to_owned(),
            )
        } else {
            UpgradeStoreError::Io(error)
        }
    })?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o022 != 0
        || metadata.len() == 0
        || metadata.len() > MAX_SPOOL_PACKAGE_BYTES
    {
        return Err(UpgradeStoreError::Package(
            "trusted package spool input has unsafe metadata".to_owned(),
        ));
    }
    Ok(())
}

fn load_fingerprint(path: &Path) -> Result<String, UpgradeStoreError> {
    let metadata = fs::symlink_metadata(path).map_err(|error| {
        if error.kind() == io::ErrorKind::NotFound {
            UpgradeStoreError::Package(
                "pinned release key fingerprint is not provisioned".to_owned(),
            )
        } else {
            UpgradeStoreError::Io(error)
        }
    })?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o777 != 0o600
        || metadata.len() != 65
    {
        return Err(UpgradeStoreError::Package(
            "pinned release key fingerprint must be an operator-owned mode 0600 65-byte file"
                .to_owned(),
        ));
    }
    let mut value = String::new();
    File::open(path)
        .map_err(UpgradeStoreError::Io)?
        .read_to_string(&mut value)
        .map_err(UpgradeStoreError::Io)?;
    let value = value.trim_end_matches('\n').to_owned();
    if value.len() != 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(UpgradeStoreError::Package(
            "pinned release key fingerprint must be 64 lowercase hexadecimal characters".to_owned(),
        ));
    }
    Ok(value)
}

fn validate_public_key(path: &Path) -> Result<(), UpgradeStoreError> {
    let metadata = fs::symlink_metadata(path).map_err(|error| {
        if error.kind() == io::ErrorKind::NotFound {
            UpgradeStoreError::Package("release signing public key is not provisioned".to_owned())
        } else {
            UpgradeStoreError::Io(error)
        }
    })?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.len() < 48
        || metadata.len() > 4096
        || metadata.permissions().mode() & 0o022 != 0
    {
        return Err(UpgradeStoreError::Package(
            "release signing public key has unsafe metadata".to_owned(),
        ));
    }
    Ok(())
}

fn validate_verified_marker(
    package_root: &Path,
    archive_digest: &[u8; 32],
) -> Result<(), UpgradeStoreError> {
    let marker = package_root.join(".ocservia-package-verified");
    let metadata = fs::symlink_metadata(&marker)
        .map_err(|_| UpgradeStoreError::Package("verified package marker is missing".to_owned()))?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.uid() != trusted_euid()
        || metadata.nlink() != 1
        || metadata.permissions().mode() & 0o777 != 0o600
    {
        return Err(UpgradeStoreError::Package(
            "verified package marker has unsafe metadata".to_owned(),
        ));
    }
    let marker = fs::read_to_string(&marker).map_err(UpgradeStoreError::Io)?;
    let lines: Vec<&str> = marker.trim_end_matches('\n').split('\n').collect();
    if lines.len() != 3
        || lines[0] != "version=1"
        || lines[1] != format!("archive_sha256={}", hex::encode(archive_digest))
        || lines[2]
            != format!(
                "package={}",
                package_root
                    .file_name()
                    .and_then(|name| name.to_str())
                    .unwrap_or_default()
            )
    {
        return Err(UpgradeStoreError::Package(
            "verified package marker does not bind the authorized archive".to_owned(),
        ));
    }
    Ok(())
}

fn ensure_operations_root(operations_dir: &Path) -> Result<(), UpgradeStoreError> {
    if !operations_dir.is_absolute()
        || operations_dir
            .components()
            .any(|component| !matches!(component, Component::Normal(_) | Component::RootDir))
    {
        return Err(UpgradeStoreError::Unsafe(
            "durable operations path must be absolute and canonical",
        ));
    }
    // The deepest already-existing ancestor becomes the trust base; every
    // component below it is created mode 0700 and revalidated.
    let mut ancestor = operations_dir;
    while fs::symlink_metadata(ancestor).is_err() {
        ancestor = ancestor.parent().ok_or(UpgradeStoreError::Unsafe(
            "durable operations path has no anchor",
        ))?;
    }
    validate_directory_strict(ancestor)?;
    let missing = operations_dir
        .strip_prefix(ancestor)
        .map_err(|_| UpgradeStoreError::Unsafe("durable operations path escapes its anchor"))?;
    let mut current = PathBuf::from(ancestor);
    for component in missing.components() {
        let Component::Normal(name) = component else {
            return Err(UpgradeStoreError::Unsafe(
                "durable operations path must be canonical",
            ));
        };
        current.push(name);
        match fs::DirBuilder::new()
            .recursive(false)
            .mode(0o700)
            .create(&current)
        {
            Ok(()) => {}
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
            Err(error) => return Err(UpgradeStoreError::Io(error)),
        }
        validate_directory_strict(&current)?;
    }
    Ok(())
}

fn create_operation_dir(operation_dir: &Path) -> Result<(), UpgradeStoreError> {
    match fs::DirBuilder::new()
        .recursive(false)
        .mode(0o700)
        .create(operation_dir)
    {
        Ok(()) => {}
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
            return Err(UpgradeStoreError::Unsafe(
                "durable operation directory appeared concurrently",
            ));
        }
        Err(error) => return Err(UpgradeStoreError::Io(error)),
    }
    validate_directory_strict(operation_dir)
}

/// Which lock owner an atomic temporary belongs to. The scheduler and the
/// runner may hold their own locks on the same operation directory at the
/// same time, so each side names - and therefore only ever cleans up - its
/// own temporaries.
#[derive(Clone, Copy, Debug)]
enum TempOwner {
    Schedule,
    Run,
}

impl TempOwner {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Schedule => "schedule",
            Self::Run => "run",
        }
    }
}

fn write_atomic(path: &Path, bytes: &[u8], owner: TempOwner) -> Result<(), UpgradeStoreError> {
    let directory = path.parent().ok_or(UpgradeStoreError::Unsafe(
        "durable record path needs a parent",
    ))?;
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or(UpgradeStoreError::Unsafe(
            "durable record name must be UTF-8",
        ))?;
    let temporary = directory.join(format!(".{name}.{}.tmp.{}", owner.as_str(), Uuid::now_v7()));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&temporary)
        .map_err(UpgradeStoreError::Io)?;
    file.write_all(bytes).map_err(UpgradeStoreError::Io)?;
    file.sync_all().map_err(UpgradeStoreError::Io)?;
    drop(file);
    fs::rename(&temporary, path).map_err(UpgradeStoreError::Io)?;
    sync_directory(directory)
}

fn cleanup_stale_temporaries(
    operation_dir: &Path,
    owner: TempOwner,
) -> Result<(), UpgradeStoreError> {
    match fs::symlink_metadata(operation_dir) {
        Ok(_) => validate_directory_strict(operation_dir)?,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(UpgradeStoreError::Io(error)),
    }
    let entries = fs::read_dir(operation_dir).map_err(UpgradeStoreError::Io)?;
    let mut removed = false;
    for entry in entries {
        let entry = entry.map_err(UpgradeStoreError::Io)?;
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            continue;
        };
        let stale = ["intent", "state", "result"].iter().any(|record| {
            let prefix = format!(".{record}.{}.tmp", owner.as_str());
            if name == prefix {
                return true;
            }
            name.strip_prefix(&format!("{prefix}."))
                .is_some_and(|suffix| {
                    Uuid::parse_str(suffix)
                        .is_ok_and(|id| id.get_version_num() == 7 && id.to_string() == suffix)
                })
        });
        if !stale {
            continue;
        }
        validate_record_file(&entry.path())?;
        fs::remove_file(entry.path()).map_err(UpgradeStoreError::Io)?;
        removed = true;
    }
    if removed {
        sync_directory(operation_dir)?;
    }
    Ok(())
}

fn sync_directory(directory: &Path) -> Result<(), UpgradeStoreError> {
    File::open(directory)
        .and_then(|handle| handle.sync_all())
        .map_err(UpgradeStoreError::Io)
}

fn sha256_file(path: &Path) -> Result<[u8; 32], UpgradeStoreError> {
    let mut file = File::open(path).map_err(UpgradeStoreError::Io)?;
    let mut hasher = Sha256::new();
    let mut buffer = vec![0_u8; 64 * 1024];
    loop {
        let read = file.read(&mut buffer).map_err(UpgradeStoreError::Io)?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(hasher.finalize().into())
}

fn sha256_if_present(path: &Path) -> Result<Option<[u8; 32]>, UpgradeStoreError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => {
            Ok(Some(sha256_file(path)?))
        }
        Ok(_) => Ok(None),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(UpgradeStoreError::Io(error)),
    }
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt as _;

    use super::*;

    fn test_root(tag: &str) -> PathBuf {
        let root = std::env::temp_dir().join(format!("ocservia-upgrader-{tag}-{}", Uuid::now_v7()));
        fs::create_dir(&root).expect("create test root");
        fs::set_permissions(&root, fs::Permissions::from_mode(0o700)).expect("secure test root");
        root
    }

    fn operations_dir(root: &Path) -> PathBuf {
        root.join("var/lib/ocservia-upgrade/operations")
    }

    fn scheduler(root: &Path) -> UpgradeScheduler {
        UpgradeScheduler::new(operations_dir(root), UpgradeTrigger::Disabled)
    }

    fn new_intent(package_sha256: [u8; 32]) -> UpgradeIntent {
        UpgradeIntent::new(
            *Uuid::now_v7().as_bytes(),
            *Uuid::now_v7().as_bytes(),
            "1.2.3",
            package_sha256,
            ocservia_contracts::agent_upgrade::runtime_architecture().expect("host architecture"),
            [0x55; 32],
        )
        .expect("valid test intent")
    }

    fn record_mode(path: &Path) -> u32 {
        fs::symlink_metadata(path)
            .expect("record metadata")
            .permissions()
            .mode()
    }

    fn force_state(root: &Path, operation: &Uuid, state: OperationState) {
        write_atomic(
            &operations_dir(root)
                .join(operation.to_string())
                .join("state"),
            format!("{}\n", state.as_str()).as_bytes(),
            TempOwner::Run,
        )
        .expect("force durable state");
    }

    fn plant_stale_temporary(path: &Path, bytes: &[u8]) {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(path)
            .expect("create stale temporary");
        file.write_all(bytes).expect("write stale temporary");
        file.sync_all().expect("sync stale temporary");
    }

    #[test]
    fn scheduling_is_idempotent_and_binds_the_release_identity() {
        let root = test_root("idempotent");
        let intent = new_intent([0x43; 32]);
        assert_eq!(
            scheduler(&root)
                .schedule_and_trigger(&intent)
                .expect("first schedule"),
            ScheduleOutcome::Scheduled
        );
        assert_eq!(
            scheduler(&root)
                .schedule_and_trigger(&intent)
                .expect("duplicate schedule"),
            ScheduleOutcome::AlreadyScheduled
        );
        let operation_dir = operations_dir(&root).join(intent.operation_id.to_string());
        assert_eq!(record_mode(&operation_dir.join("intent")) & 0o777, 0o600);
        assert_eq!(record_mode(&operation_dir.join("state")) & 0o777, 0o600);
        assert!(
            fs::symlink_metadata(&operation_dir)
                .expect("operation directory")
                .is_dir()
        );

        let mut conflicting = intent.clone();
        conflicting.package_sha256 = [0x44; 32];
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&conflicting),
            Err(UpgradeStoreError::IdentityConflict)
        ));
        // The conflicting delivery cannot rewrite the immutable intent.
        let persisted = load_intent(&operation_dir).expect("persisted intent");
        assert_eq!(persisted.package_sha256, [0x43; 32]);
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn scheduling_replays_while_execution_lock_is_held() {
        let root = test_root("replay-during-execution");
        let intent = new_intent([0x45; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("initial schedule");
        let _execution =
            UpgradeLock::acquire_execution(&operations_dir(&root)).expect("runner execution lock");

        assert_eq!(
            scheduler(&root)
                .schedule_and_trigger(&intent)
                .expect("exact replay while runner is active"),
            ScheduleOutcome::AlreadyScheduled
        );
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn scheduling_completes_crashed_partial_commits_safely() {
        let root = test_root("partial");
        let intent = new_intent([1; 32]);
        let operations = operations_dir(&root);
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(operations.join(intent.operation_id.to_string()))
            .expect("crashed operation directory");
        assert_eq!(
            scheduler(&root)
                .schedule_and_trigger(&intent)
                .expect("resume"),
            ScheduleOutcome::Scheduled
        );

        let second = new_intent([2; 32]);
        let second_dir = operations.join(second.operation_id.to_string());
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&second_dir)
            .expect("second operation directory");
        write_atomic(
            &second_dir.join("intent"),
            second.render().as_bytes(),
            TempOwner::Schedule,
        )
        .expect("crashed after intent commit");
        assert_eq!(
            scheduler(&root)
                .schedule_and_trigger(&second)
                .expect("complete state commit"),
            ScheduleOutcome::Scheduled
        );

        let mut conflicting = second.clone();
        conflicting.target_version = "1.2.4".to_owned();
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&conflicting),
            Err(UpgradeStoreError::IdentityConflict)
        ));

        let orphan = new_intent([3; 32]);
        let orphan_dir = operations.join(orphan.operation_id.to_string());
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&orphan_dir)
            .expect("orphan directory");
        write_atomic(
            &orphan_dir.join("state"),
            b"accepted\n",
            TempOwner::Schedule,
        )
        .expect("orphan state");
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&orphan),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn scheduling_recovers_stale_state_temporary_before_rename() {
        let root = test_root("stale-state");
        let intent = new_intent([0x31; 32]);
        let operation_dir = operations_dir(&root).join(intent.operation_id.to_string());
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&operation_dir)
            .expect("operation directory");
        write_atomic(
            &operation_dir.join("intent"),
            intent.render().as_bytes(),
            TempOwner::Schedule,
        )
        .expect("committed intent");
        let stale = operation_dir.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
        plant_stale_temporary(&stale, b"accepted\n");

        assert_eq!(
            scheduler(&root)
                .schedule_and_trigger(&intent)
                .expect("resume after interrupted rename"),
            ScheduleOutcome::Scheduled
        );
        assert!(!stale.exists());
        assert_eq!(
            load_state(&operation_dir).expect("recovered state"),
            OperationState::Accepted
        );
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn scheduler_cleanup_leaves_runner_temporaries_alone() {
        let root = test_root("temp-ownership");
        let intent = new_intent([0x61; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let operation_dir = operations_dir(&root).join(intent.operation_id.to_string());
        // The runner owns these between create and rename while it executes
        // the upgrade; the scheduler's replay cleanup must never remove them.
        let runner_state = operation_dir.join(format!(".state.run.tmp.{}", Uuid::now_v7()));
        let runner_result = operation_dir.join(format!(".result.run.tmp.{}", Uuid::now_v7()));
        plant_stale_temporary(&runner_state, b"running\n");
        plant_stale_temporary(&runner_result, b"partial result");
        let scheduler_state = operation_dir.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
        plant_stale_temporary(&scheduler_state, b"accepted\n");

        assert_eq!(
            scheduler(&root)
                .schedule_and_trigger(&intent)
                .expect("exact replay"),
            ScheduleOutcome::AlreadyScheduled
        );
        assert!(runner_state.exists());
        assert!(runner_result.exists());
        assert!(!scheduler_state.exists());
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn only_one_active_operation_is_scheduled_per_node() {
        let root = test_root("active");
        let first = new_intent([4; 32]);
        scheduler(&root)
            .schedule_and_trigger(&first)
            .expect("first operation");
        let second = new_intent([5; 32]);
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&second),
            Err(UpgradeStoreError::ActiveConflict)
        ));
        force_state(&root, &first.operation_id, OperationState::Succeeded);
        scheduler(&root)
            .schedule_and_trigger(&second)
            .expect("terminal operation frees the node");
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn concurrent_scheduling_commits_exactly_one_active_operation() {
        use std::sync::{Arc, Barrier};

        let root = test_root("concurrent-active");
        let first = new_intent([0x41; 32]);
        let second = new_intent([0x42; 32]);
        let barrier = Arc::new(Barrier::new(3));
        let handles = [first, second].map(|intent| {
            let root = root.clone();
            let barrier = Arc::clone(&barrier);
            std::thread::spawn(move || {
                barrier.wait();
                scheduler(&root).schedule_and_trigger(&intent)
            })
        });
        barrier.wait();
        let outcomes = handles.map(|handle| handle.join().expect("scheduler thread"));
        assert_eq!(
            outcomes
                .iter()
                .filter(|outcome| matches!(outcome, Ok(ScheduleOutcome::Scheduled)))
                .count(),
            1
        );
        assert_eq!(
            outcomes
                .iter()
                .filter(|outcome| matches!(outcome, Err(UpgradeStoreError::ActiveConflict)))
                .count(),
            1
        );
        let active = fs::read_dir(operations_dir(&root))
            .expect("operations root")
            .filter_map(Result::ok)
            .filter(|entry| Uuid::parse_str(&entry.file_name().to_string_lossy()).is_ok())
            .filter(|entry| {
                matches!(
                    load_state(&entry.path()),
                    Ok(OperationState::Accepted | OperationState::Running)
                )
            })
            .count();
        assert_eq!(active, 1);
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn concurrent_same_intent_is_exact_replay() {
        use std::sync::{Arc, Barrier};

        let root = test_root("concurrent-replay");
        let intent = new_intent([0x51; 32]);
        let barrier = Arc::new(Barrier::new(3));
        let handles = [intent.clone(), intent].map(|intent| {
            let root = root.clone();
            let barrier = Arc::clone(&barrier);
            std::thread::spawn(move || {
                barrier.wait();
                scheduler(&root).schedule_and_trigger(&intent)
            })
        });
        barrier.wait();
        let outcomes = handles.map(|handle| handle.join().expect("scheduler thread"));
        assert_eq!(
            outcomes
                .iter()
                .filter(|outcome| matches!(outcome, Ok(ScheduleOutcome::Scheduled)))
                .count(),
            1
        );
        assert_eq!(
            outcomes
                .iter()
                .filter(|outcome| matches!(outcome, Ok(ScheduleOutcome::AlreadyScheduled)))
                .count(),
            1
        );
        assert_eq!(
            outcomes
                .iter()
                .filter(|outcome| matches!(outcome, Err(UpgradeStoreError::ActiveConflict)))
                .count(),
            0
        );
        let durable = fs::read_dir(operations_dir(&root))
            .expect("operations root")
            .filter_map(Result::ok)
            .filter(|entry| Uuid::parse_str(&entry.file_name().to_string_lossy()).is_ok())
            .count();
        assert_eq!(durable, 1);
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn terminal_history_does_not_block_new_scheduling() {
        let root = test_root("terminal-history");
        for index in 0..65 {
            let intent = new_intent([index; 32]);
            scheduler(&root)
                .schedule_and_trigger(&intent)
                .expect("schedule historical operation");
            force_state(&root, &intent.operation_id, OperationState::Succeeded);
        }
        scheduler(&root)
            .schedule_and_trigger(&new_intent([0xff; 32]))
            .expect("schedule after retained terminal history");
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn durable_records_reject_filesystem_substitution() {
        let root = test_root("substitution");
        let intent = new_intent([6; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let operation_dir = operations_dir(&root).join(intent.operation_id.to_string());
        let intent_path = operation_dir.join("intent");
        let state_path = operation_dir.join("state");
        let trusted = fs::read(&intent_path).expect("trusted intent bytes");

        let substitution = test_root("substitution-target");
        let outside = substitution.join("outside");
        fs::write(&outside, "attacker").expect("outside target");

        fs::remove_file(&intent_path).expect("remove intent");
        std::os::unix::fs::symlink(&outside, &intent_path).expect("plant symlink");
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&intent),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::remove_file(&intent_path).expect("remove symlink");
        write_atomic(&intent_path, &trusted, TempOwner::Schedule).expect("restore intent");

        fs::remove_file(&state_path).expect("remove state");
        fs::write(&state_path, "accepted\n").expect("loose state");
        fs::set_permissions(&state_path, fs::Permissions::from_mode(0o644)).expect("unsafe mode");
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&intent),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::set_permissions(&state_path, fs::Permissions::from_mode(0o600)).expect("restore mode");

        let hardlink = operation_dir.join("state.link");
        fs::hard_link(&state_path, &hardlink).expect("plant hardlink");
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&intent),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::remove_file(&hardlink).expect("remove hardlink");
        fs::remove_dir_all(&root).expect("cleanup");
        fs::remove_dir_all(&substitution).expect("cleanup");
    }

    #[test]
    fn unsafe_durable_ancestry_is_rejected() {
        let root = test_root("ancestry");
        let intent = new_intent([7; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let operations = operations_dir(&root);
        fs::set_permissions(&operations, fs::Permissions::from_mode(0o0770))
            .expect("group writable");
        assert!(matches!(
            scheduler(&root).schedule_and_trigger(&new_intent([8; 32])),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::set_permissions(&operations, fs::Permissions::from_mode(0o700)).expect("restore");
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn runner_rejects_malformed_operation_identities() {
        let runner = UpgradeRunner::new(PathBuf::from("/"));
        for malformed in [
            "../etc/shadow",
            "018f0c2e-7b1a-7c3d-8e9f-0123456789aB",
            "018f0c2e7b1a7c3d8e9f0123456789ab",
            "not-a-uuid",
            "",
        ] {
            assert!(
                matches!(runner.run(malformed), Err(UpgradeStoreError::Invalid(_))),
                "identity refused: {malformed}"
            );
        }
    }

    #[test]
    fn runner_replays_terminal_states_without_side_effects() {
        let root = test_root("terminal");
        let intent = new_intent([9; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        force_state(&root, &intent.operation_id, OperationState::Succeeded);
        let runner = UpgradeRunner::new(root.clone());
        assert_eq!(
            runner
                .run(&intent.operation_id.to_string())
                .expect("terminal replay"),
            OperationState::Succeeded
        );
        force_state(&root, &intent.operation_id, OperationState::RolledBack);
        assert_eq!(
            runner
                .run(&intent.operation_id.to_string())
                .expect("rolled back replay"),
            OperationState::RolledBack
        );
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn runner_persists_failure_when_the_spool_cannot_resolve_the_release() {
        let root = test_root("missing-spool");
        let intent = new_intent([0x0a; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let runner = UpgradeRunner::new(root.clone());
        let failure = runner
            .run(&intent.operation_id.to_string())
            .expect_err("spool is empty");
        assert!(matches!(failure, UpgradeStoreError::Package(_)));
        let operation_dir = operations_dir(&root).join(intent.operation_id.to_string());
        assert_eq!(
            load_state(&operation_dir).expect("durable failure state"),
            OperationState::Failed
        );
        let result = fs::read_to_string(operation_dir.join("result")).expect("failure evidence");
        assert!(result.contains("state=failed"));
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn runner_recovers_stale_result_temporary_before_rename() {
        let root = test_root("stale-result");
        let intent = new_intent([0x0b; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let operation_dir = operations_dir(&root).join(intent.operation_id.to_string());
        let stale = operation_dir.join(format!(".result.run.tmp.{}", Uuid::now_v7()));
        plant_stale_temporary(&stale, b"incomplete result");
        let scheduler_state = operation_dir.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
        plant_stale_temporary(&scheduler_state, b"accepted\n");

        let failure = UpgradeRunner::new(root.clone())
            .run(&intent.operation_id.to_string())
            .expect_err("empty spool fails after recovery");
        assert!(matches!(failure, UpgradeStoreError::Package(_)));
        assert!(!stale.exists());
        assert!(scheduler_state.exists());
        assert_eq!(
            load_state(&operation_dir).expect("terminal state"),
            OperationState::Failed
        );
        assert!(operation_dir.join("result").exists());
        fs::remove_dir_all(&root).expect("cleanup");
    }

    struct FakePackage {
        root: PathBuf,
        archive_digest: [u8; 32],
        lifecycle_runs: PathBuf,
    }

    fn fake_lifecycle_tree(tag: &str) -> FakePackage {
        fake_lifecycle_tree_with_version(tag, "1.2.3")
    }

    fn fake_lifecycle_tree_with_version(tag: &str, version: &str) -> FakePackage {
        let root = test_root(tag);
        let architecture =
            ocservia_contracts::agent_upgrade::runtime_architecture().expect("host architecture");
        let spool = root.join("var/lib/ocservia-upgrade/package-spool");
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&spool)
            .expect("spool");
        let archive_bytes = format!("fake-agent-package-{tag}").into_bytes();
        let archive_digest: [u8; 32] = Sha256::digest(&archive_bytes).into();
        let archive_name = format!("ocservia-agent-{version}-linux-{architecture}.tar.gz");
        let archive = spool.join(&archive_name);
        fs::write(&archive, &archive_bytes).expect("archive");
        fs::write(
            spool.join(format!("{archive_name}.sha256")),
            format!("{}  {archive_name}\n", hex::encode(archive_digest)),
        )
        .expect("checksum");
        fs::write(
            spool.join(format!("{archive_name}.sha256.sig")),
            b"signature",
        )
        .expect("signature");
        for file in [
            archive.clone(),
            spool.join(format!("{archive_name}.sha256")),
            spool.join(format!("{archive_name}.sha256.sig")),
        ] {
            fs::set_permissions(&file, fs::Permissions::from_mode(0o644)).expect("spool mode");
        }

        let keys = root.join("etc/ocservia");
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o755)
            .create(&keys)
            .expect("release key directory");
        fs::write(
            keys.join("release-signing.pub.pem"),
            b"-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAfakefixturekeymaterialpadding\n-----END PUBLIC KEY-----\n",
        )
        .expect("release key");
        let fingerprint = keys.join("trusted-release-key.sha256");
        fs::write(&fingerprint, format!("{}\n", "ab".repeat(32))).expect("fingerprint");
        fs::set_permissions(&fingerprint, fs::Permissions::from_mode(0o600))
            .expect("fingerprint mode");

        let verifier = root.join("usr/libexec/ocservia/ocservia-agent-verify");
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o755)
            .create(verifier.parent().expect("verifier directory"))
            .expect("libexec");
        let lifecycle_runs = root.join("lifecycle-runs");
        let verifier_source = "#!/bin/sh
set -e
digest=$(cut -d' ' -f1 \"$2\")
stage=\"${DESTDIR}/var/lib/ocservia-upgrade/package-staging/pkg.1\"
pkg=\"${stage}/extracted/ocservia-agent-@VERSION@\"
mkdir -p \"${pkg}/rust/target/release\" \"${pkg}/scripts\"
printf 'new-agent-@TAG@\n' > \"${pkg}/rust/target/release/ocservia-agent\"
printf 'new-privd-@TAG@\n' > \"${pkg}/rust/target/release/ocservia-privd\"
{
  printf '#!/bin/sh\nset -e\n'
  printf 'here=$(dirname \"$0\")\n'
  printf 'cp \"${here}/../rust/target/release/ocservia-agent\" \"${DESTDIR}/usr/libexec/ocservia/ocservia-agent\"\n'
  printf 'cp \"${here}/../rust/target/release/ocservia-privd\" \"${DESTDIR}/usr/libexec/ocservia/ocservia-privd\"\n'
  printf 'echo \"archive_sha256=%s\" > \"${DESTDIR}/var/lib/ocservia-upgrade/installed-commit\"\\n' \"${digest}\"
  printf 'printf x >> \"@LIFECYCLE_LOG@\"\n'
} > \"${pkg}/scripts/upgrade-agent.sh\"
chmod 0755 \"${pkg}/rust/target/release/ocservia-agent\" \"${pkg}/rust/target/release/ocservia-privd\" \"${pkg}/scripts/upgrade-agent.sh\"
printf 'version=1\narchive_sha256=%s\npackage=ocservia-agent-@VERSION@\n' \"${digest}\" > \"${pkg}/.ocservia-package-verified\"
chmod 0600 \"${pkg}/.ocservia-package-verified\"
chmod 0700 \"${pkg}\"
echo \"${pkg}\"
"
        .replace("@TAG@", tag)
        .replace("@VERSION@", version)
        .replace("@LIFECYCLE_LOG@", &lifecycle_runs.display().to_string());
        fs::write(&verifier, verifier_source).expect("verifier script");
        fs::write(&lifecycle_runs, b"").expect("lifecycle counter");
        fs::set_permissions(&verifier, fs::Permissions::from_mode(0o755)).expect("verifier mode");
        FakePackage {
            root,
            archive_digest,
            lifecycle_runs,
        }
    }

    #[test]
    fn runner_executes_the_verified_lifecycle_exactly_once() {
        let package = fake_lifecycle_tree("lifecycle");
        let intent = new_intent(package.archive_digest);
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let runner = UpgradeRunner::new(package.root.clone());
        assert_eq!(
            runner
                .run(&intent.operation_id.to_string())
                .expect("lifecycle completes"),
            OperationState::Succeeded
        );
        let installed_agent =
            fs::read(package.root.join("usr/libexec/ocservia/ocservia-agent")).expect("installed");
        assert_eq!(installed_agent, b"new-agent-lifecycle\n");
        let operation_dir = operations_dir(&package.root).join(intent.operation_id.to_string());
        assert_eq!(
            load_state(&operation_dir).expect("final state"),
            OperationState::Succeeded
        );
        assert!(
            fs::read_to_string(operation_dir.join("result"))
                .expect("result evidence")
                .contains("state=succeeded")
        );
        assert_eq!(
            fs::read_to_string(&package.lifecycle_runs).expect("lifecycle counter"),
            "x"
        );

        // Crash window after replacement and result loss: the durable state
        // says running and the authorized binaries are already installed, so
        // the retry must converge without re-running the destructive backup
        // lifecycle.
        force_state(&package.root, &intent.operation_id, OperationState::Running);
        assert_eq!(
            runner
                .run(&intent.operation_id.to_string())
                .expect("converge without reinstall"),
            OperationState::Succeeded
        );
        assert_eq!(
            fs::read_to_string(&package.lifecycle_runs).expect("lifecycle counter"),
            "x"
        );
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    /// Stages the crash window the review pinned: an interrupted lifecycle
    /// already replaced some host files, but the installation commit record
    /// was never written.
    fn seed_partial_install(package: &FakePackage, tag: &str, agent: bool, privd: bool) {
        if agent {
            fs::write(
                package.root.join("usr/libexec/ocservia/ocservia-agent"),
                format!("new-agent-{tag}\n"),
            )
            .expect("seed agent");
        }
        if privd {
            fs::write(
                package.root.join("usr/libexec/ocservia/ocservia-privd"),
                format!("new-privd-{tag}\n"),
            )
            .expect("seed privd");
        }
    }

    fn seed_commit_record(package: &FakePackage, digest_hex: &str) {
        fs::write(
            package
                .root
                .join("var/lib/ocservia-upgrade/installed-commit"),
            format!("archive_sha256={digest_hex}\n"),
        )
        .expect("seed commit record");
    }

    fn assert_interrupted_install_refused(package: &FakePackage, intent: &UpgradeIntent) {
        let runner = UpgradeRunner::new(package.root.clone());
        let failure = runner
            .run(&intent.operation_id.to_string())
            .expect_err("partial install must be refused");
        assert!(matches!(failure, UpgradeStoreError::Unsafe(_)));
        let operation_dir = operations_dir(&package.root).join(intent.operation_id.to_string());
        assert_eq!(
            load_state(&operation_dir).expect("durable failure"),
            OperationState::Failed
        );
        let result = fs::read_to_string(operation_dir.join("result")).expect("result evidence");
        assert!(result.contains("state=failed"));
        assert!(result.contains("interrupted before its commit record"));
        // The refusal happens before any lifecycle side effect, so the
        // rollback snapshot from the interrupted attempt is never overwritten.
        assert_eq!(
            fs::read_to_string(&package.lifecycle_runs).expect("lifecycle counter"),
            ""
        );
    }

    #[test]
    fn runner_refuses_a_crash_after_both_binaries_without_the_commit_record() {
        // Power loss after agent and privd were replaced but before the
        // upgrader, verifier, units, and commit record existed: the mixed
        // host must never converge to success or re-run the backup step.
        let package = fake_lifecycle_tree("crash-both");
        seed_partial_install(&package, "crash-both", true, true);
        let intent = new_intent(package.archive_digest);
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        assert_interrupted_install_refused(&package, &intent);
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn runner_refuses_a_crash_after_the_agent_binary_alone() {
        // Power loss between replacing the agent and the privd: still an
        // interrupted install that must refuse rather than snapshot the
        // mixed pair as the previous release.
        let package = fake_lifecycle_tree("crash-agent");
        seed_partial_install(&package, "crash-agent", true, false);
        let intent = new_intent(package.archive_digest);
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        assert_interrupted_install_refused(&package, &intent);
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn runner_refuses_matching_binaries_behind_a_foreign_commit_record() {
        // Binaries of this package are installed, but the commit record
        // names a different package: the record is the only convergence
        // proof, so a mismatch stays an interrupted install.
        let package = fake_lifecycle_tree("foreign-record");
        seed_partial_install(&package, "foreign-record", true, true);
        let mut foreign = package.archive_digest;
        foreign[0] ^= 0xff;
        seed_commit_record(&package, &hex::encode(foreign));
        let intent = new_intent(package.archive_digest);
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        assert_interrupted_install_refused(&package, &intent);
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn runner_converges_only_through_the_matching_commit_record() {
        // The complete install finished and recorded its commit, but the
        // result persistence was lost: the retry converges through the
        // digest-bound record without re-running the lifecycle.
        let package = fake_lifecycle_tree("commit-converge");
        seed_partial_install(&package, "commit-converge", true, true);
        seed_commit_record(&package, &hex::encode(package.archive_digest));
        let intent = new_intent(package.archive_digest);
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let runner = UpgradeRunner::new(package.root.clone());
        assert_eq!(
            runner
                .run(&intent.operation_id.to_string())
                .expect("converge through the commit record"),
            OperationState::Succeeded
        );
        assert_eq!(
            fs::read_to_string(&package.lifecycle_runs).expect("lifecycle counter"),
            ""
        );
        let operation_dir = operations_dir(&package.root).join(intent.operation_id.to_string());
        assert!(
            fs::read_to_string(operation_dir.join("result"))
                .expect("result evidence")
                .contains("verified package already installed")
        );
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn runner_refuses_a_replay_without_the_commit_record() {
        // A lifecycle that installed the binaries but lost (or never wrote)
        // its commit record cannot be certified on replay: the record is
        // the only convergence proof, so the retry fails closed.
        let package = fake_lifecycle_tree("missing-record");
        // Sabotage only the record: run the lifecycle once through the
        // normal path, then remove the record and replay as if the crash
        // lost it before the runner could verify.
        let intent = new_intent(package.archive_digest);
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let runner = UpgradeRunner::new(package.root.clone());
        assert_eq!(
            runner
                .run(&intent.operation_id.to_string())
                .expect("lifecycle completes"),
            OperationState::Succeeded
        );
        fs::remove_file(
            package
                .root
                .join("var/lib/ocservia-upgrade/installed-commit"),
        )
        .expect("drop commit record");
        force_state(&package.root, &intent.operation_id, OperationState::Running);
        let failure = runner
            .run(&intent.operation_id.to_string())
            .expect_err("missing commit record");
        assert!(matches!(failure, UpgradeStoreError::Unsafe(_)));
        assert_eq!(
            load_state(&operations_dir(&package.root).join(intent.operation_id.to_string()))
                .expect("durable failure"),
            OperationState::Failed
        );
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn runner_refuses_a_spool_digest_the_intent_does_not_authorize() {
        let package = fake_lifecycle_tree("digest");
        let mut digest = package.archive_digest;
        digest[0] ^= 0xff;
        let intent = new_intent(digest);
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let runner = UpgradeRunner::new(package.root.clone());
        let failure = runner
            .run(&intent.operation_id.to_string())
            .expect_err("digest mismatch");
        assert!(matches!(failure, UpgradeStoreError::Package(_)));
        let operation_dir = operations_dir(&package.root).join(intent.operation_id.to_string());
        assert_eq!(
            load_state(&operation_dir).expect("durable failure"),
            OperationState::Failed
        );
        assert!(
            !package
                .lifecycle_runs
                .metadata()
                .is_ok_and(|meta| meta.len() > 0)
        );
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn runner_refuses_an_architecture_the_host_does_not_run() {
        let package = fake_lifecycle_tree("foreign-arch");
        let foreign = if ocservia_contracts::agent_upgrade::runtime_architecture() == Some("amd64")
        {
            "arm64"
        } else {
            "amd64"
        };
        let intent = UpgradeIntent::new(
            *Uuid::now_v7().as_bytes(),
            *Uuid::now_v7().as_bytes(),
            "1.2.3",
            package.archive_digest,
            foreign,
            [0x55; 32],
        )
        .expect("foreign intent");
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let runner = UpgradeRunner::new(package.root.clone());
        let failure = runner
            .run(&intent.operation_id.to_string())
            .expect_err("foreign architecture");
        assert!(matches!(failure, UpgradeStoreError::Package(_)));
        let operation_dir = operations_dir(&package.root).join(intent.operation_id.to_string());
        assert_eq!(
            load_state(&operation_dir).expect("durable failure"),
            OperationState::Failed
        );
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn runner_refuses_a_target_not_newer_than_the_running_release() {
        // "0.0.0" predates every publishable release, so the fence refuses
        // regardless of the identity embedded in this test build.
        let package = fake_lifecycle_tree_with_version("downgrade-fence", "0.0.0");
        let intent = UpgradeIntent::new(
            *Uuid::now_v7().as_bytes(),
            *Uuid::now_v7().as_bytes(),
            "0.0.0",
            package.archive_digest,
            ocservia_contracts::agent_upgrade::runtime_architecture().expect("host architecture"),
            [0x55; 32],
        )
        .expect("downgrade intent");
        scheduler(&package.root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let runner = UpgradeRunner::new(package.root.clone());
        let failure = runner
            .run(&intent.operation_id.to_string())
            .expect_err("stale target version");
        assert!(matches!(failure, UpgradeStoreError::Package(_)));
        let operation_dir = operations_dir(&package.root).join(intent.operation_id.to_string());
        assert_eq!(
            load_state(&operation_dir).expect("durable failure"),
            OperationState::Failed
        );
        let result = fs::read_to_string(operation_dir.join("result")).expect("result evidence");
        assert!(result.contains("state=failed"));
        assert!(result.contains("not newer than the running release"));
        // The refusal happens before any lifecycle side effect.
        assert_eq!(
            fs::read_to_string(&package.lifecycle_runs).expect("lifecycle counter"),
            ""
        );
        fs::remove_dir_all(&package.root).expect("cleanup");
    }

    #[test]
    fn read_recent_results_reports_terminal_outcomes_newest_first() {
        let root = test_root("read-results");
        let first = new_intent([0x43; 32]);
        let second = new_intent([0x44; 32]);
        let in_flight = new_intent([0x45; 32]);
        let operations = operations_dir(&root);
        // The durable store admits one active operation at a time, so each
        // predecessor is terminal before the next is scheduled.
        scheduler(&root)
            .schedule_and_trigger(&first)
            .expect("schedule first");
        write_terminal(
            &operations.join(first.operation_id.to_string()),
            &first,
            OperationState::Succeeded,
            "activated release",
        )
        .expect("first terminal outcome");
        scheduler(&root)
            .schedule_and_trigger(&second)
            .expect("schedule second");
        write_terminal(
            &operations.join(second.operation_id.to_string()),
            &second,
            OperationState::Failed,
            "spool could not resolve the release",
        )
        .expect("second terminal outcome");
        scheduler(&root)
            .schedule_and_trigger(&in_flight)
            .expect("schedule in-flight");

        let results = read_recent_results(&operations, 8).expect("read recent results");
        assert_eq!(results.len(), 2, "in-flight operation carries no evidence");
        assert_eq!(results[0].operation_id, second.operation_id);
        assert_eq!(results[0].state, OperationState::Failed);
        assert!(results[0].completed_unix > 0);
        assert_eq!(results[1].operation_id, first.operation_id);
        assert_eq!(results[1].state, OperationState::Succeeded);
        assert_eq!(results[1].target_version, "1.2.3");
        assert_eq!(results[1].detail, "activated release");
        let bounded = read_recent_results(&operations, 1).expect("bounded read");
        assert_eq!(bounded.len(), 1, "the report window stays bounded");
        assert_eq!(bounded[0].operation_id, second.operation_id);
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn read_recent_results_fails_closed_on_identity_drift() {
        let root = test_root("read-results-drift");
        let intent = new_intent([0x46; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let mut drifted = intent.clone();
        drifted.operation_id = Uuid::now_v7();
        write_terminal(
            &operations_dir(&root).join(intent.operation_id.to_string()),
            &drifted,
            OperationState::Succeeded,
            "drifted identity",
        )
        .expect("terminal outcome");
        assert!(matches!(
            read_recent_results(&operations_dir(&root), 8),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::remove_dir_all(&root).expect("cleanup");
    }

    #[test]
    fn read_recent_results_rejects_unsafe_present_record() {
        let root = test_root("read-results-unsafe-record");
        let intent = new_intent([0x47; 32]);
        scheduler(&root)
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let operation_dir = operations_dir(&root).join(intent.operation_id.to_string());
        write_terminal(
            &operation_dir,
            &intent,
            OperationState::Succeeded,
            "activated release",
        )
        .expect("terminal outcome");
        fs::set_permissions(
            operation_dir.join("result"),
            fs::Permissions::from_mode(0o644),
        )
        .expect("make result metadata unsafe");
        assert!(matches!(
            read_recent_results(&operations_dir(&root), 8),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::remove_dir_all(&root).expect("cleanup");
    }
}
