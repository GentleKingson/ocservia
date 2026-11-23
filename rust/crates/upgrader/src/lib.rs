//! Durable, root-owned self-upgrade boundary shared by privd and the
//! standalone `ocservia-upgrader` runner.
//!
//! privd never executes an upgrade itself. After independently verifying a
//! Controller-signed `AgentUpgrade` command it commits an immutable intent
//! below a fixed root-only hierarchy and starts the fixed
//! `ocservia-upgrader@<operation>.service` unit. The standalone runner then
//! verifies the authorized native package, applies it through dpkg or RPM,
//! and persists the durable local result that survives the
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

use rustix::fs::{FlockOperation, Mode, OFlags};
use rustix::io::Errno;
use sha2::{Digest as _, Sha256};
use uuid::Uuid;

/// Fixed schema version of the durable intent and result records.
pub const RECORD_SCHEMA_VERSION: u32 = 1;
/// Fixed root-only durable operations hierarchy.
pub const DEFAULT_OPERATIONS_DIR: &str = "/var/lib/ocservia-upgrade/operations";
/// Fixed operator-provisioned trusted package spool.
pub const DEFAULT_SPOOL_DIR: &str = "/var/lib/ocservia-upgrade/package-spool";
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
                    if self.active_sibling(Some(intent.operation_id))?.is_some() {
                        return Err(UpgradeStoreError::ActiveConflict);
                    }
                    write_atomic(
                        &operation_dir.join("state"),
                        format!("{}\n", OperationState::Accepted.as_str()).as_bytes(),
                        TempOwner::Schedule,
                    )?;
                    ScheduleOutcome::Scheduled
                }
                OperationPieces::Complete {
                    intent: existing, ..
                } => {
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
            if let OperationPieces::Complete {
                state: OperationState::Accepted | OperationState::Running,
                ..
            } = read_operation_pieces(&entry.path())?
            {
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
        state: OperationState,
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
    let present = |path: &Path| match fs::symlink_metadata(path) {
        Ok(_) => Ok(true),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error),
    };
    let intent_present = present(&intent_path)?;
    let state_present = present(&state_path)?;
    match (intent_present, state_present) {
        (false, false) => Ok(OperationPieces::Partial { intent: None }),
        (true, false) => Ok(OperationPieces::Partial {
            intent: Some(load_intent(operation_dir)?),
        }),
        (true, true) => Ok(OperationPieces::Complete {
            intent: load_intent(operation_dir)?,
            state: load_state(operation_dir)?,
        }),
        (false, true) => Err(UpgradeStoreError::Unsafe(
            "durable operation state exists without its immutable intent",
        )),
    }
}

fn load_intent(operation_dir: &Path) -> Result<UpgradeIntent, UpgradeStoreError> {
    UpgradeIntent::parse(&read_record(&operation_dir.join("intent"), 1024)?)
}

fn load_state(operation_dir: &Path) -> Result<OperationState, UpgradeStoreError> {
    OperationState::parse(&read_record(&operation_dir.join("state"), 64)?)
}

fn read_record(path: &Path, limit: usize) -> Result<Vec<u8>, UpgradeStoreError> {
    let fd = rustix::fs::open(
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(path_error)?;
    let file = File::from(fd);
    validate_record_metadata(&file.metadata()?)?;
    let mut bytes = Vec::new();
    file.take(limit as u64 + 1).read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(UpgradeStoreError::Unsafe(
            "durable record exceeds its bound",
        ));
    }
    Ok(bytes)
}

fn optional_text_record(path: &Path, limit: usize) -> Result<Option<String>, UpgradeStoreError> {
    match read_record(path, limit) {
        Ok(bytes) => String::from_utf8(bytes)
            .map(Some)
            .map_err(|_| UpgradeStoreError::Unsafe("durable record is not UTF-8")),
        Err(UpgradeStoreError::Io(error)) if error.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error),
    }
}

fn path_error(error: Errno) -> UpgradeStoreError {
    if error == Errno::LOOP || error == Errno::NOTDIR {
        return UpgradeStoreError::Unsafe(
            "trusted upgrade path contains a symlink or unexpected file",
        );
    }
    UpgradeStoreError::Io(io::Error::from(error))
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
    match open_operations_root(operations_dir, false) {
        Ok(()) => {}
        Err(UpgradeStoreError::Io(error)) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(results);
        }
        Err(error) => return Err(error),
    }
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
        let bytes = match read_record(&record_dir.join("result"), 1024) {
            Err(UpgradeStoreError::Io(error)) if error.kind() == io::ErrorKind::NotFound => {
                continue;
            }
            result => result?,
        };
        let OperationPieces::Complete { intent, .. } = read_operation_pieces(&record_dir)? else {
            return Err(UpgradeStoreError::Unsafe(
                "durable result has no committed operation",
            ));
        };
        if intent.operation_id != candidate {
            return Err(UpgradeStoreError::Unsafe(
                "durable intent operation identity does not match its directory",
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
    pub fn operations_dir(&self) -> PathBuf {
        self.root.join("var/lib/ocservia-upgrade/operations")
    }

    #[must_use]
    pub fn spool_dir(&self) -> PathBuf {
        self.root.join(DEFAULT_SPOOL_DIR.trim_start_matches('/'))
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
        open_operations_root(&operations_root, false)?;
        let operation_dir = operations_root.join(operation_id.to_string());
        validate_directory_strict(&operation_dir)?;
        let intent = load_intent(&operation_dir)?;
        if intent.operation_id != operation_id {
            return Err(UpgradeStoreError::Unsafe(
                "durable intent operation identity does not match its directory",
            ));
        }
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

    fn execute(
        &self,
        operation_dir: &Path,
        intent: &UpgradeIntent,
    ) -> Result<OperationState, UpgradeStoreError> {
        let (state, detail) = match self.apply_native(operation_dir, intent) {
            Ok(result) => result,
            Err(failure) => return refuse(operation_dir, intent, failure),
        };
        write_terminal(operation_dir, intent, state, &detail)?;
        Ok(state)
    }

    fn apply_native(
        &self,
        operation_dir: &Path,
        intent: &UpgradeIntent,
    ) -> Result<(OperationState, String), UpgradeStoreError> {
        if ocservia_contracts::agent_upgrade::runtime_architecture()
            != Some(intent.architecture.as_str())
        {
            return Err(UpgradeStoreError::Package(
                "intent architecture mismatches this host".into(),
            ));
        }
        let manager = if self.root.join("var/lib/dpkg/status").is_file() {
            NativePackage::Deb
        } else {
            NativePackage::Rpm
        };
        let spool = self.spool_dir();
        ensure_operations_root(&spool)?;
        let previous_file = operation_dir.join("previous-version");
        let previous_version = if let Some(version) = optional_text_record(&previous_file, 128)? {
            version
        } else {
            let version = self.installed_version(manager)?;
            write_atomic(&previous_file, version.as_bytes(), TempOwner::Run)?;
            version
        };
        if !ocservia_contracts::agent_upgrade::valid_target_version(&previous_version) {
            return Err(UpgradeStoreError::Unsafe("invalid cached package version"));
        }
        let previous = spool.join(manager.filename(&previous_version, &intent.architecture));
        open_spool_file(&previous)?;
        let rollback_file = operation_dir.join("rollback-reason");
        let failure = if let Some(reason) = optional_text_record(&rollback_file, MAX_DETAIL_BYTES)?
        {
            reason
        } else {
            let target = self.authorized_package(manager, intent)?;
            write_atomic(&operation_dir.join("state"), b"running\n", TempOwner::Run)?;
            // Native package replacement is replayable. Preserve the original baseline
            // even if dpkg or RPM was interrupted halfway through the transaction.
            match self.install_and_probe(manager, &target, &intent.target_version) {
                Ok(()) => {
                    return Ok((
                        OperationState::Succeeded,
                        "native package installed and services active".into(),
                    ));
                }
                Err(error) => {
                    let detail = bounded_detail(&error.to_string());
                    write_atomic(&rollback_file, detail.as_bytes(), TempOwner::Run)?;
                    detail
                }
            }
        };
        self.install_and_probe(manager, &previous, &previous_version)
            .map_err(|error| {
                UpgradeStoreError::Lifecycle(format!(
                    "rollback failed: {error}; upgrade failure: {failure}"
                ))
            })?;
        Ok((OperationState::RolledBack, failure))
    }

    fn authorized_package(
        &self,
        manager: NativePackage,
        intent: &UpgradeIntent,
    ) -> Result<PathBuf, UpgradeStoreError> {
        let name = manager.filename(&intent.target_version, &intent.architecture);
        let target = self.spool_dir().join(&name);
        if target.try_exists()? {
            verify_digest(&target, intent)?;
        } else {
            let url = format!(
                "https://github.com/GentleKingson/ocservia/releases/download/v{}/{}",
                intent.target_version, name
            );
            let temporary = self
                .spool_dir()
                .join(format!(".download-{}", intent.operation_id));
            let download = (|| {
                checked(
                    Command::new(self.root.join("usr/bin/curl"))
                        .args([
                            "--fail",
                            "--silent",
                            "--show-error",
                            "--location",
                            "--proto",
                            "=https",
                            "--proto-redir",
                            "=https",
                            "--max-time",
                            "600",
                            "--max-filesize",
                            &MAX_SPOOL_PACKAGE_BYTES.to_string(),
                            "--output",
                        ])
                        .arg(&temporary)
                        .arg(url),
                )?;
                verify_digest(&temporary, intent)?.sync_all()?;
                fs::rename(&temporary, &target)?;
                sync_directory(&self.spool_dir())
            })();
            if download.is_err() && temporary.try_exists()? {
                fs::remove_file(temporary)?;
            }
            download?;
        }
        Ok(target)
    }

    fn installed_version(&self, manager: NativePackage) -> Result<String, UpgradeStoreError> {
        let record = match manager {
            NativePackage::Deb => {
                checked(Command::new(self.root.join("usr/bin/dpkg-query")).args([
                    "-W",
                    "-f=${Status} ${Version}",
                    "ocservia-agent",
                ]))?
            }
            NativePackage::Rpm => checked(Command::new(self.root.join("usr/bin/rpm")).args([
                "-q",
                "--qf",
                "%{VERSION}-%{RELEASE}",
                "ocservia-agent",
            ]))?,
        };
        let version = match manager {
            NativePackage::Deb => record.strip_prefix("install ok installed "),
            NativePackage::Rpm => Some(record.as_str()),
        }
        .and_then(|s| s.strip_suffix("-1"))
        .map(|s| s.replace("~rc.", "-rc."));
        version
            .filter(|s| ocservia_contracts::agent_upgrade::valid_target_version(s))
            .ok_or_else(|| {
                UpgradeStoreError::Package(
                    "installed Agent does not have a supported native package version".into(),
                )
            })
    }

    fn install_and_probe(
        &self,
        manager: NativePackage,
        package: &Path,
        version: &str,
    ) -> Result<(), UpgradeStoreError> {
        match manager {
            NativePackage::Deb => checked(
                Command::new(self.root.join("usr/bin/dpkg"))
                    .arg("-i")
                    .arg(package),
            )?,
            NativePackage::Rpm => checked(
                Command::new(self.root.join("usr/bin/rpm"))
                    .args(["-U", "--oldpackage", "--replacepkgs"])
                    .arg(package),
            )?,
        };
        if self.installed_version(manager)? != version {
            return Err(UpgradeStoreError::Lifecycle(
                "installed native package version mismatches intent".into(),
            ));
        }
        let systemctl = self.root.join("usr/bin/systemctl");
        let units = ["ocservia-privd.service", "ocservia-agent.service"];
        checked(Command::new(&systemctl).arg("start").args(units))?;
        // Give both daemons time to run their startup probes. Query individually:
        // systemctl is-active with multiple arguments succeeds if *any* is active.
        std::thread::sleep(std::time::Duration::from_secs(2));
        for unit in units {
            checked(Command::new(&systemctl).args(["is-active", "--quiet", unit]))?;
        }
        Ok(())
    }
}

#[derive(Clone, Copy)]
enum NativePackage {
    Deb,
    Rpm,
}

impl NativePackage {
    fn filename(self, version: &str, arch: &str) -> String {
        match self {
            Self::Deb => format!("ocservia-agent_{version}-1_{arch}.deb"),
            Self::Rpm => {
                let rpm_arch = if arch == "amd64" { "x86_64" } else { "aarch64" };
                format!("ocservia-agent-{version}-1.{rpm_arch}.rpm")
            }
        }
    }
}

fn verify_digest(path: &Path, intent: &UpgradeIntent) -> Result<File, UpgradeStoreError> {
    let mut file = open_spool_file(path)?;
    if sha256_reader(&mut file)? != intent.package_sha256 {
        return Err(UpgradeStoreError::Package(
            "native package digest mismatches authorized intent".into(),
        ));
    }
    Ok(file)
}

fn checked(command: &mut Command) -> Result<String, UpgradeStoreError> {
    let output = command.output()?;
    if !output.status.success() {
        return Err(UpgradeStoreError::Lifecycle(format!(
            "{} exited with {}: {}",
            command.get_program().to_string_lossy(),
            output.status,
            bounded_detail(&String::from_utf8_lossy(&output.stderr))
        )));
    }
    Ok(String::from_utf8_lossy(&output.stdout)
        .trim_end()
        .to_owned())
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
        rustix::fs::flock(&file, FlockOperation::NonBlockingLockExclusive).map_err(|error| {
            if error == Errno::WOULDBLOCK {
                UpgradeStoreError::ActiveConflict
            } else {
                UpgradeStoreError::Io(io::Error::from(error))
            }
        })?;
        Ok(Self { _file: file })
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
    validate_record_metadata(&fs::symlink_metadata(path)?)
}

fn validate_record_metadata(metadata: &fs::Metadata) -> Result<(), UpgradeStoreError> {
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

fn open_spool_file(path: &Path) -> Result<File, UpgradeStoreError> {
    let fd = rustix::fs::open(
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|error| {
        if error == Errno::NOENT {
            UpgradeStoreError::Package(
                "trusted package spool does not provide the authorized release".to_owned(),
            )
        } else {
            path_error(error)
        }
    })?;
    let file = File::from(fd);
    let metadata = file.metadata()?;
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
    Ok(file)
}

fn ensure_operations_root(operations_dir: &Path) -> Result<(), UpgradeStoreError> {
    open_operations_root(operations_dir, true)
}

fn open_operations_root(operations_dir: &Path, create: bool) -> Result<(), UpgradeStoreError> {
    if !operations_dir.is_absolute()
        || operations_dir
            .components()
            .any(|component| !matches!(component, Component::Normal(_) | Component::RootDir))
    {
        return Err(UpgradeStoreError::Unsafe(
            "durable operations path must be absolute and canonical",
        ));
    }
    let flags = OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC;
    let mut dir = rustix::fs::open("/", flags, Mode::empty()).map_err(path_error)?;
    let mut components = operations_dir.components().skip(1).peekable();
    while let Some(Component::Normal(name)) = components.next() {
        let next = match rustix::fs::openat(&dir, name, flags, Mode::empty()) {
            Err(Errno::NOENT) if create => {
                match rustix::fs::mkdirat(&dir, name, Mode::RWXU) {
                    Ok(()) | Err(Errno::EXIST) => {}
                    Err(error) => return Err(path_error(error)),
                }
                rustix::fs::openat(&dir, name, flags, Mode::empty()).map_err(path_error)?
            }
            result => result.map_err(path_error)?,
        };
        let metadata = rustix::fs::fstat(&next).map_err(path_error)?;
        let last = components.peek().is_none();
        let owner = metadata.st_uid;
        let mode = metadata.st_mode;
        let protected_ancestor = !last && owner == 0 && mode & 0o1000 != 0;
        if (owner != trusted_euid() && (last || owner != 0))
            || (mode & 0o022 != 0 && !protected_ancestor)
        {
            return Err(UpgradeStoreError::Unsafe(
                "trusted upgrade path ancestry must be operator-owned real directories",
            ));
        }
        dir = next;
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
        let stale = [
            "intent",
            "state",
            "result",
            "previous-version",
            "rollback-reason",
        ]
        .iter()
        .any(|record| {
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

fn sha256_reader(file: &mut File) -> Result<[u8; 32], UpgradeStoreError> {
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

#[cfg(test)]
#[allow(clippy::too_many_lines)]
mod tests {
    use std::sync::{Arc, Barrier};

    use super::*;

    struct TempRoot(PathBuf);
    impl TempRoot {
        fn new(tag: &str) -> Self {
            let p =
                std::env::temp_dir().join(format!("ocservia-upgrader-{tag}-{}", Uuid::now_v7()));
            fs::create_dir(&p).expect("mkdir");
            fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).expect("700");
            Self(p)
        }
        fn path(&self) -> &Path {
            &self.0
        }
        fn ops(&self) -> PathBuf {
            self.0.join("var/lib/ocservia-upgrade/operations")
        }
        fn scheduler(&self) -> UpgradeScheduler {
            UpgradeScheduler::new(self.ops(), UpgradeTrigger::Disabled)
        }
        fn runner(&self) -> UpgradeRunner {
            UpgradeRunner::new(self.0.clone())
        }
        fn force_state(&self, op: &Uuid, state: OperationState) {
            write_atomic(
                &self.ops().join(op.to_string()).join("state"),
                format!("{}\n", state.as_str()).as_bytes(),
                TempOwner::Run,
            )
            .expect("force");
        }
    }
    impl Drop for TempRoot {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn new_intent(sha: [u8; 32]) -> UpgradeIntent {
        new_intent_ver(
            "1.2.3",
            sha,
            ocservia_contracts::agent_upgrade::runtime_architecture().expect("arch"),
        )
    }
    fn new_intent_ver(ver: &str, sha: [u8; 32], arch: &str) -> UpgradeIntent {
        UpgradeIntent::new(
            *Uuid::now_v7().as_bytes(),
            *Uuid::now_v7().as_bytes(),
            ver,
            sha,
            arch,
            [0x55; 32],
        )
        .expect("intent")
    }
    fn record_mode(path: &Path) -> u32 {
        fs::symlink_metadata(path)
            .expect("meta")
            .permissions()
            .mode()
            & 0o777
    }
    fn plant_tmp(path: &Path, bytes: &[u8]) {
        let mut f = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(path)
            .expect("tmp");
        f.write_all(bytes).expect("w");
        f.sync_all().expect("sync");
    }

    #[test]
    fn scheduling_idempotency_conflict_lock_and_active_slot() {
        let root = TempRoot::new("sched");
        let intent = new_intent([0x43; 32]);
        assert_eq!(
            root.scheduler().schedule_and_trigger(&intent).expect("1st"),
            ScheduleOutcome::Scheduled
        );
        assert_eq!(
            root.scheduler().schedule_and_trigger(&intent).expect("dup"),
            ScheduleOutcome::AlreadyScheduled
        );
        let op_dir = root.ops().join(intent.operation_id.to_string());
        assert_eq!(
            (
                record_mode(&op_dir.join("intent")),
                record_mode(&op_dir.join("state"))
            ),
            (0o600, 0o600)
        );
        assert!(fs::symlink_metadata(&op_dir).expect("dir").is_dir());

        let mut conflict = intent.clone();
        conflict.package_sha256 = [0x44; 32];
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&conflict),
            Err(UpgradeStoreError::IdentityConflict)
        ));
        assert_eq!(
            load_intent(&op_dir).expect("persisted").package_sha256,
            [0x43; 32]
        );

        // Exact replay while execution lock is held; another active op is rejected until terminal.
        {
            let _exec = UpgradeLock::acquire_execution(&root.ops()).expect("lock");
            assert_eq!(
                root.scheduler()
                    .schedule_and_trigger(&intent)
                    .expect("replay"),
                ScheduleOutcome::AlreadyScheduled
            );
        }
        let second = new_intent([0x45; 32]);
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&second),
            Err(UpgradeStoreError::ActiveConflict)
        ));
        root.force_state(&intent.operation_id, OperationState::Succeeded);
        root.scheduler()
            .schedule_and_trigger(&second)
            .expect("freed");
        root.force_state(&second.operation_id, OperationState::Succeeded);

        // Large terminal history does not block scheduling.
        for i in 0..65 {
            let h = new_intent([i; 32]);
            root.scheduler().schedule_and_trigger(&h).expect("hist");
            root.force_state(&h.operation_id, OperationState::Succeeded);
        }
        root.scheduler()
            .schedule_and_trigger(&new_intent([0xff; 32]))
            .expect("after history");
    }

    #[test]
    fn partial_operations_do_not_block_new_work_or_result_reads() {
        let root = TempRoot::new("partial-sibling");
        let partial = new_intent([1; 32]);
        let partial_dir = root.ops().join(partial.operation_id.to_string());
        ensure_operations_root(&partial_dir).expect("operation directory");
        assert!(
            read_recent_results(&root.ops(), 1)
                .expect("read partial")
                .is_empty()
        );
        let current = new_intent([2; 32]);
        root.scheduler()
            .schedule_and_trigger(&current)
            .expect("schedule past partial sibling");
        write_atomic(
            &partial_dir.join("intent"),
            partial.render().as_bytes(),
            TempOwner::Schedule,
        )
        .expect("persist partial intent");
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&partial),
            Err(UpgradeStoreError::ActiveConflict)
        ));
        assert!(
            read_recent_results(&root.ops(), 1)
                .expect("read partial intent")
                .is_empty()
        );
    }

    #[test]
    fn scheduling_crash_recovery_and_temp_ownership() {
        let root = TempRoot::new("partial");
        let first = new_intent([1; 32]);
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(root.ops().join(first.operation_id.to_string()))
            .expect("d1");
        assert_eq!(
            root.scheduler()
                .schedule_and_trigger(&first)
                .expect("resume"),
            ScheduleOutcome::Scheduled
        );
        root.force_state(&first.operation_id, OperationState::Succeeded);

        let second = new_intent([2; 32]);
        let d2 = root.ops().join(second.operation_id.to_string());
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&d2)
            .expect("d2");
        write_atomic(
            &d2.join("intent"),
            second.render().as_bytes(),
            TempOwner::Schedule,
        )
        .expect("intent");
        let stale = d2.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
        plant_tmp(&stale, b"accepted\n");
        assert_eq!(
            root.scheduler()
                .schedule_and_trigger(&second)
                .expect("state"),
            ScheduleOutcome::Scheduled
        );
        assert!(!stale.exists());
        assert_eq!(load_state(&d2).expect("st"), OperationState::Accepted);

        let mut conflict = second.clone();
        conflict.target_version = "1.2.4".into();
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&conflict),
            Err(UpgradeStoreError::IdentityConflict)
        ));

        // Runner temporaries are preserved while scheduler temporaries are cleaned on replay.
        let r_state = d2.join(format!(".state.run.tmp.{}", Uuid::now_v7()));
        let r_res = d2.join(format!(".result.run.tmp.{}", Uuid::now_v7()));
        let s_state = d2.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
        plant_tmp(&r_state, b"running\n");
        plant_tmp(&r_res, b"partial");
        plant_tmp(&s_state, b"accepted\n");
        assert_eq!(
            root.scheduler()
                .schedule_and_trigger(&second)
                .expect("replay"),
            ScheduleOutcome::AlreadyScheduled
        );
        assert!(r_state.exists() && r_res.exists() && !s_state.exists());
        root.force_state(&second.operation_id, OperationState::Succeeded);

        // Orphan state without intent fails closed.
        let orphan = new_intent([3; 32]);
        let od = root.ops().join(orphan.operation_id.to_string());
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&od)
            .expect("od");
        write_atomic(&od.join("state"), b"accepted\n", TempOwner::Schedule).expect("os");
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&orphan),
            Err(UpgradeStoreError::Unsafe(_))
        ));
    }

    #[test]
    fn concurrent_scheduling_enforces_single_active_and_exact_replay() {
        for same_intent in [false, true] {
            let root = TempRoot::new(if same_intent { "conc-rep" } else { "conc-act" });
            let a = new_intent([0x41; 32]);
            let b = if same_intent {
                a.clone()
            } else {
                new_intent([0x42; 32])
            };
            let barrier = Arc::new(Barrier::new(3));
            let handles = [a, b].map(|intent| {
                let ops = root.ops();
                let br = Arc::clone(&barrier);
                std::thread::spawn(move || {
                    br.wait();
                    UpgradeScheduler::new(ops, UpgradeTrigger::Disabled)
                        .schedule_and_trigger(&intent)
                })
            });
            barrier.wait();
            let outs = handles.map(|h| h.join().expect("join"));
            assert_eq!(
                outs.iter()
                    .filter(|o| matches!(o, Ok(ScheduleOutcome::Scheduled)))
                    .count(),
                1
            );
            if same_intent {
                assert_eq!(
                    outs.iter()
                        .filter(|o| matches!(o, Ok(ScheduleOutcome::AlreadyScheduled)))
                        .count(),
                    1
                );
            } else {
                assert_eq!(
                    outs.iter()
                        .filter(|o| matches!(o, Err(UpgradeStoreError::ActiveConflict)))
                        .count(),
                    1
                );
            }
            let active = fs::read_dir(root.ops())
                .expect("rd")
                .filter_map(Result::ok)
                .filter(|e| Uuid::parse_str(&e.file_name().to_string_lossy()).is_ok())
                .filter(|e| {
                    matches!(
                        load_state(&e.path()),
                        Ok(OperationState::Accepted | OperationState::Running)
                    )
                })
                .count();
            assert_eq!(active, 1);
        }
    }

    #[test]
    fn filesystem_substitution_and_unsafe_ancestry_are_rejected() {
        let root = TempRoot::new("subst");
        let sub = TempRoot::new("subst-target");
        let intent = new_intent([6; 32]);
        root.scheduler()
            .schedule_and_trigger(&intent)
            .expect("sched");
        let op_dir = root.ops().join(intent.operation_id.to_string());
        let (ip, sp) = (op_dir.join("intent"), op_dir.join("state"));
        let trusted = fs::read(&ip).expect("read");
        let outside = sub.path().join("outside");
        fs::write(&outside, "attacker").expect("w");

        fs::remove_file(&ip).expect("rm");
        std::os::unix::fs::symlink(&outside, &ip).expect("sym");
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&intent),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::remove_file(&ip).expect("rm");
        write_atomic(&ip, &trusted, TempOwner::Schedule).expect("restore");

        fs::set_permissions(&sp, fs::Permissions::from_mode(0o644)).expect("644");
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&intent),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::set_permissions(&sp, fs::Permissions::from_mode(0o600)).expect("600");

        let hl = op_dir.join("state.link");
        fs::hard_link(&sp, &hl).expect("hl");
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&intent),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::remove_file(&hl).expect("rm");

        fs::set_permissions(root.ops(), fs::Permissions::from_mode(0o770)).expect("770");
        assert!(matches!(
            root.scheduler().schedule_and_trigger(&new_intent([8; 32])),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::set_permissions(root.ops(), fs::Permissions::from_mode(0o700)).expect("700");
    }

    #[test]
    fn runner_input_validation_terminal_replay_and_spool_failure_recovery() {
        let sys_runner = UpgradeRunner::new(PathBuf::from("/"));
        for bad in [
            "../etc/shadow",
            "018f0c2e-7b1a-7c3d-8e9f-0123456789aB",
            "018f0c2e7b1a7c3d8e9f0123456789ab",
            "not-a-uuid",
            "",
        ] {
            assert!(
                matches!(sys_runner.run(bad), Err(UpgradeStoreError::Invalid(_))),
                "{bad}"
            );
        }

        let root = TempRoot::new("term-spool");
        let t_intent = new_intent([9; 32]);
        root.scheduler().schedule_and_trigger(&t_intent).expect("s");
        for state in [OperationState::Succeeded, OperationState::RolledBack] {
            root.force_state(&t_intent.operation_id, state);
            assert_eq!(
                root.runner()
                    .run(&t_intent.operation_id.to_string())
                    .expect("rep"),
                state
            );
        }

        let f_intent = new_intent([0x0b; 32]);
        root.scheduler().schedule_and_trigger(&f_intent).expect("s");
        let op_dir = root.ops().join(f_intent.operation_id.to_string());
        let stale = op_dir.join(format!(".result.run.tmp.{}", Uuid::now_v7()));
        let sched_tmp = op_dir.join(format!(".state.schedule.tmp.{}", Uuid::now_v7()));
        plant_tmp(&stale, b"incomplete");
        plant_tmp(&sched_tmp, b"accepted\n");
        assert!(matches!(
            root.runner().run(&f_intent.operation_id.to_string()),
            Err(UpgradeStoreError::Io(_))
        ));
        assert!(!stale.exists() && sched_tmp.exists());
        assert_eq!(load_state(&op_dir).expect("st"), OperationState::Failed);
        assert!(
            fs::read_to_string(op_dir.join("result"))
                .expect("res")
                .contains("state=failed")
        );
    }

    #[test]
    fn native_install_replays_preserve_rollback_package_and_probe_each_service() {
        let arch = ocservia_contracts::agent_upgrade::runtime_architecture().expect("architecture");
        let root = TempRoot::new("native");
        let runner = root.runner();
        let spool = runner.spool_dir();
        ensure_operations_root(&spool).expect("cache");
        let status = root.path().join("var/lib/dpkg/status");
        fs::create_dir_all(status.parent().unwrap()).expect("dpkg db");
        fs::write(&status, b"1.0.0").expect("installed");
        let bin = root.path().join("usr/bin");
        fs::create_dir_all(&bin).expect("bin");
        for (name, commands) in [
            (
                "dpkg-query",
                "printf 'install ok installed %s-1' \"$(cat \"$status\")\"",
            ),
            ("dpkg", "test \"$1\" = -i && cat \"$2\" > \"$status\""),
            (
                "systemctl",
                "test \"$1\" != restart; test \"$1\" != is-active || test \"$3\" != ocservia-agent.service || test ! -f \"$status.fail\" || test \"$(cat \"$status\")\" = 1.0.0",
            ),
        ] {
            let path = bin.join(name);
            fs::write(
                &path,
                format!(
                    "#!/bin/sh\nset -eu\nstatus='{}'\n{commands}\n",
                    status.display()
                ),
            )
            .expect("test tool");
            fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).expect("executable");
        }
        let old = spool.join(NativePackage::Deb.filename("1.0.0", arch));
        let target = spool.join(NativePackage::Deb.filename("1.2.3", arch));
        fs::write(&old, b"1.0.0").unwrap();
        fs::write(&target, b"1.2.3").unwrap();
        let intent = new_intent(
            sha256_reader(&mut open_spool_file(&target).expect("target"))
                .expect("authorized digest"),
        );
        root.scheduler()
            .schedule_and_trigger(&intent)
            .expect("schedule");
        let operation = intent.operation_id.to_string();
        assert_eq!(
            runner.run(&operation).expect("upgrade"),
            OperationState::Succeeded
        );
        assert_eq!(fs::read_to_string(&status).unwrap(), "1.2.3");
        root.force_state(&intent.operation_id, OperationState::Running);
        fs::write(status.with_extension("fail"), b"").expect("fail target health");
        assert_eq!(
            runner.run(&operation).expect("rollback"),
            OperationState::RolledBack
        );
        assert_eq!(fs::read_to_string(&status).unwrap(), "1.0.0");
        assert_eq!(
            runner.run(&operation).expect("terminal replay"),
            OperationState::RolledBack
        );

        let unauthorized = new_intent([0; 32]);
        root.scheduler()
            .schedule_and_trigger(&unauthorized)
            .expect("schedule");
        assert!(matches!(
            runner.run(&unauthorized.operation_id.to_string()),
            Err(UpgradeStoreError::Package(_))
        ));
        assert_eq!(fs::read_to_string(&status).unwrap(), "1.0.0");
        assert_eq!(
            NativePackage::Rpm.filename("2.0.0-rc.1", "arm64"),
            "ocservia-agent-2.0.0-rc.1-1.aarch64.rpm"
        );
    }

    #[test]
    fn read_recent_results_ordering_drift_and_permissions() {
        let root = TempRoot::new("recent");
        let (a, b, inflight) = (
            new_intent([0x43; 32]),
            new_intent([0x44; 32]),
            new_intent([0x45; 32]),
        );
        root.scheduler().schedule_and_trigger(&a).expect("a");
        write_terminal(
            &root.ops().join(a.operation_id.to_string()),
            &a,
            OperationState::Succeeded,
            "activated release",
        )
        .expect("ta");
        root.scheduler().schedule_and_trigger(&b).expect("b");
        write_terminal(
            &root.ops().join(b.operation_id.to_string()),
            &b,
            OperationState::Failed,
            "spool missing",
        )
        .expect("tb");
        root.scheduler().schedule_and_trigger(&inflight).expect("c");

        let res = read_recent_results(&root.ops(), 8).expect("res");
        assert_eq!(res.len(), 2);
        assert_eq!(
            (res[0].operation_id, res[0].state),
            (b.operation_id, OperationState::Failed)
        );
        assert!(res[0].completed_unix > 0);
        assert_eq!(
            (
                res[1].operation_id,
                res[1].state,
                res[1].target_version.as_str(),
                res[1].detail.as_str()
            ),
            (
                a.operation_id,
                OperationState::Succeeded,
                "1.2.3",
                "activated release"
            )
        );
        let bounded = read_recent_results(&root.ops(), 1).expect("b");
        assert_eq!(
            (bounded.len(), bounded[0].operation_id),
            (1, b.operation_id)
        );

        // Unsafe record mode fails closed.
        let a_res = root.ops().join(a.operation_id.to_string()).join("result");
        fs::set_permissions(&a_res, fs::Permissions::from_mode(0o644)).expect("644");
        assert!(matches!(
            read_recent_results(&root.ops(), 8),
            Err(UpgradeStoreError::Unsafe(_))
        ));
        fs::set_permissions(&a_res, fs::Permissions::from_mode(0o600)).expect("600");

        // Identity drift fails closed.
        let mut drifted = a.clone();
        drifted.operation_id = Uuid::now_v7();
        write_terminal(
            &root.ops().join(a.operation_id.to_string()),
            &drifted,
            OperationState::Succeeded,
            "drift",
        )
        .expect("td");
        assert!(matches!(
            read_recent_results(&root.ops(), 8),
            Err(UpgradeStoreError::Unsafe(_))
        ));
    }
}
