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

mod fs;
mod runner;
mod scheduler;
#[cfg(test)]
#[allow(clippy::too_many_lines)]
mod tests;

use crate::fs::{
    decode_record_lines, expect_line, parse_line_digest, parse_line_uuid, parse_line_value,
    require_uuid_v7,
};
pub use crate::runner::UpgradeRunner;
#[cfg(test)]
pub use crate::scheduler::read_recent_results;
pub use crate::scheduler::{DurableUpgradeResult, UpgradeScheduler, read_recent_results_resilient};
use std::io;
use uuid::Uuid;

/// Fixed schema version of the durable intent and result records.
pub const RECORD_SCHEMA_VERSION: u32 = 1;
/// Fixed root-only durable operations hierarchy.
pub const DEFAULT_OPERATIONS_DIR: &str = "/var/lib/ocservia-upgrade/operations";
/// Fixed operator-provisioned trusted package spool.
pub const DEFAULT_SPOOL_DIR: &str = "/var/lib/ocservia-upgrade/package-spool";
/// Fixed systemd template unit started per durable upgrade operation.
pub const UPGRADE_UNIT_TEMPLATE: &str = "ocservia-upgrader@{operation}.service";

pub(crate) const MAX_DETAIL_BYTES: usize = 160;
pub(crate) const MAX_SPOOL_PACKAGE_BYTES: u64 = 2 * 1024 * 1024 * 1024;
pub(crate) const MAX_DOWNLOAD_ATTEMPTS: u32 = 3;
pub(crate) const DEFAULT_UPGRADE_BASE_URL: &str =
    "https://github.com/GentleKingson/ocservia/releases/download";
pub(crate) const DEFAULT_PRIVD_SOCKET_PATH: &str = "/run/ocserv-platform/privd.sock";
pub(crate) const SCHEDULE_LOCK_FILE: &str = ".schedule.lock";
pub(crate) const EXECUTION_LOCK_FILE: &str = ".run.lock";

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
