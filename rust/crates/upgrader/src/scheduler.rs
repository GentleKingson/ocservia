use std::fs::{self, File};
use std::io::{self, Read as _};
use std::path::{Path, PathBuf};
use std::process::Command;

use rustix::fs::{Mode, OFlags};
use rustix::io::Errno;
use sha2::{Digest as _, Sha256};
use uuid::Uuid;

use crate::fs::{
    TempOwner, UpgradeLock, cleanup_stale_temporaries, create_operation_dir, decode_record_lines,
    ensure_operations_root, expect_line, open_operations_root, parse_line_digest, parse_line_uuid,
    parse_line_value, validate_directory_strict, validate_record_metadata, write_atomic,
};
use crate::{
    OperationState, RECORD_SCHEMA_VERSION, ScheduleOutcome, UPGRADE_UNIT_TEMPLATE, UpgradeIntent,
    UpgradeStoreError, UpgradeTrigger,
};

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
                    commit_intent(&operation_dir, &intent_bytes)?;
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

pub(crate) enum OperationPieces {
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

pub(crate) fn commit_intent(
    operation_dir: &Path,
    intent_bytes: &[u8],
) -> Result<(), UpgradeStoreError> {
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

pub(crate) fn read_operation_pieces(
    operation_dir: &Path,
) -> Result<OperationPieces, UpgradeStoreError> {
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

pub(crate) fn load_intent(operation_dir: &Path) -> Result<UpgradeIntent, UpgradeStoreError> {
    UpgradeIntent::parse(&read_record(&operation_dir.join("intent"), 1024)?)
}

pub(crate) fn load_state(operation_dir: &Path) -> Result<OperationState, UpgradeStoreError> {
    OperationState::parse(&read_record(&operation_dir.join("state"), 64)?)
}

pub(crate) fn read_record(path: &Path, limit: usize) -> Result<Vec<u8>, UpgradeStoreError> {
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

pub(crate) fn optional_text_record(
    path: &Path,
    limit: usize,
) -> Result<Option<String>, UpgradeStoreError> {
    match read_record(path, limit) {
        Ok(bytes) => String::from_utf8(bytes)
            .map(Some)
            .map_err(|_| UpgradeStoreError::Unsafe("durable record is not UTF-8")),
        Err(UpgradeStoreError::Io(error)) if error.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error),
    }
}

pub(crate) fn path_error(error: Errno) -> UpgradeStoreError {
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
#[cfg(test)]
pub fn read_recent_results(
    operations_dir: &Path,
    limit: usize,
) -> Result<Vec<DurableUpgradeResult>, UpgradeStoreError> {
    read_recent_results_inner(operations_dir, limit, false).map(|(results, _)| results)
}

/// Reads the bounded most-recent terminal upgrade outcomes while isolating
/// individual corrupt or unsafe operation subdirectories instead of discarding
/// all historical outcomes. Returns `(results, skipped_corrupt_entries)`.
///
/// # Errors
///
/// Still fails closed if the top-level operations hierarchy itself cannot be
/// opened or fails safety validation.
pub fn read_recent_results_resilient(
    operations_dir: &Path,
    limit: usize,
) -> Result<(Vec<DurableUpgradeResult>, usize), UpgradeStoreError> {
    read_recent_results_inner(operations_dir, limit, true)
}

fn read_recent_results_inner(
    operations_dir: &Path,
    limit: usize,
    isolate_entry_errors: bool,
) -> Result<(Vec<DurableUpgradeResult>, usize), UpgradeStoreError> {
    let limit = limit.clamp(1, 32);
    let mut results = Vec::new();
    let mut skipped = 0usize;
    match open_operations_root(operations_dir, false) {
        Ok(()) => {}
        Err(UpgradeStoreError::Io(error)) if error.kind() == io::ErrorKind::NotFound => {
            return Ok((results, 0));
        }
        Err(error) => return Err(error),
    }
    let entries = match fs::read_dir(operations_dir) {
        Ok(entries) => entries,
        // A node that never ran an upgrade has no durable evidence at all.
        Err(failure) if failure.kind() == io::ErrorKind::NotFound => return Ok((results, 0)),
        Err(failure) => return Err(UpgradeStoreError::Io(failure)),
    };
    for entry in entries {
        let entry = entry.map_err(UpgradeStoreError::Io)?;
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            if isolate_entry_errors {
                skipped = skipped.saturating_add(1);
                tracing::warn!("skipping non-UTF-8 durable upgrade operation entry");
                continue;
            }
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
        match read_operation_result(&entry.path(), candidate) {
            Ok(Some(record)) => results.push(record),
            Ok(None) => {}
            Err(error) if isolate_entry_errors => {
                skipped = skipped.saturating_add(1);
                tracing::warn!(
                    operation = %candidate,
                    error = %error,
                    "skipping corrupt historical upgrade operation record"
                );
            }
            Err(error) => return Err(error),
        }
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
    Ok((results, skipped))
}

fn read_operation_result(
    record_dir: &Path,
    candidate: Uuid,
) -> Result<Option<DurableUpgradeResult>, UpgradeStoreError> {
    validate_directory_strict(record_dir)?;
    let bytes = match read_record(&record_dir.join("result"), 1024) {
        Err(UpgradeStoreError::Io(error)) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(None);
        }
        result => result?,
    };
    let OperationPieces::Complete { intent, .. } = read_operation_pieces(record_dir)? else {
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
    Ok(Some(DurableUpgradeResult {
        operation_id,
        state,
        target_version,
        package_sha256,
        completed_unix,
        detail,
        result_sha256,
    }))
}
