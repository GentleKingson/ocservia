//! Durable bounded Agent state and command-journal foundation.

#![forbid(unsafe_code)]

use std::path::Path;
use std::time::Duration;
use std::{fs, os::unix::fs::PermissionsExt as _};

use rusqlite::{Connection, OpenFlags, OptionalExtension, TransactionBehavior};

const BUSY_TIMEOUT: Duration = Duration::from_secs(5);
const OWNER_FENCE_EPOCH_FLOOR_KEY: &str = "owner_fence_epoch_floor";

/// Maximum time that telemetry remains buffered while an Agent is offline.
pub const OFFLINE_RECOVERY_RETENTION_SECONDS: u64 = 300;

/// SQLite-backed Agent local state.
#[derive(Debug)]
pub struct Journal {
    connection: Connection,
}

/// Persisted command state. A non-terminal state is never guessed to be a failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CommandState {
    Accepted,
    Running,
    Succeeded,
    Failed,
    Unknown,
}

impl CommandState {
    fn as_str(self) -> &'static str {
        match self {
            Self::Accepted => "accepted",
            Self::Running => "running",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Unknown => "unknown",
        }
    }

    fn parse(value: &str) -> Result<Self, rusqlite::Error> {
        match value {
            "accepted" => Ok(Self::Accepted),
            "running" => Ok(Self::Running),
            "succeeded" => Ok(Self::Succeeded),
            "failed" => Ok(Self::Failed),
            "unknown" => Ok(Self::Unknown),
            _ => Err(rusqlite::Error::InvalidQuery),
        }
    }
}

/// Durable command record returned for duplicate delivery and reconciliation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommandRecord {
    pub idempotency_key: [u8; 16],
    pub command_id: [u8; 16],
    pub payload_sha256: [u8; 32],
    /// Algorithm version that produced `payload_sha256` (0 = legacy, 1 = v1 canonical).
    pub payload_hash_version: i32,
    pub state: CommandState,
    pub result: Option<Vec<u8>>,
    pub error_code: Option<String>,
    /// Exact encoded root-signed receipt. Agent persists and replays it
    /// without reconstructing any attested field.
    pub privileged_result_proof: Option<Vec<u8>>,
    pub accepted_at: i64,
    pub updated_at: i64,
}

/// Outcome of durably accepting a command.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AcceptOutcome {
    Accepted(CommandRecord),
    Replay(CommandRecord),
    PayloadConflict(CommandRecord),
    IdentityConflict(CommandRecord),
}

/// Durable synthetic effect used to prove crash-safe reconciliation before real writes exist.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SyntheticEffect {
    pub payload_sha256: [u8; 32],
    pub result: Vec<u8>,
    pub executed_at: i64,
}

/// One bounded telemetry record and its local retention policy.
#[derive(Clone, Copy)]
pub struct TelemetryInsert<'a> {
    pub batch_id: &'a [u8; 16],
    pub priority: u8,
    pub observed_at: i64,
    pub expires_at: i64,
    pub payload: &'a [u8],
    pub now: i64,
    pub max_bytes: u64,
}

/// A buffered batch identifier and encoded payload.
pub type PendingTelemetry = ([u8; 16], Vec<u8>);

/// One successfully applied desired-state resource revision.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AppliedResourceRevision<'a> {
    pub resource_type: &'a str,
    pub resource_key: &'a str,
    pub revision: u64,
}

impl Journal {
    /// Opens an owner-controlled database and enforces the required `SQLite` pragmas.
    ///
    /// # Errors
    ///
    /// Returns an error when the database cannot be opened, configured, or migrated.
    pub fn open(path: &Path) -> Result<Self, rusqlite::Error> {
        let existed = path.exists();
        let connection = Connection::open_with_flags(
            path,
            OpenFlags::SQLITE_OPEN_READ_WRITE
                | OpenFlags::SQLITE_OPEN_CREATE
                | OpenFlags::SQLITE_OPEN_NO_MUTEX
                | OpenFlags::SQLITE_OPEN_NOFOLLOW,
        )?;
        if existed {
            let metadata = fs::symlink_metadata(path)
                .map_err(|error| rusqlite::Error::ToSqlConversionFailure(Box::new(error)))?;
            if metadata.file_type().is_symlink() || metadata.permissions().mode() & 0o022 != 0 {
                return Err(rusqlite::Error::InvalidPath(path.to_path_buf()));
            }
        } else {
            fs::set_permissions(path, fs::Permissions::from_mode(0o600))
                .map_err(|error| rusqlite::Error::ToSqlConversionFailure(Box::new(error)))?;
        }
        connection.busy_timeout(BUSY_TIMEOUT)?;
        connection.pragma_update(None, "journal_mode", "WAL")?;
        connection.pragma_update(None, "synchronous", "FULL")?;
        connection.pragma_update(None, "foreign_keys", true)?;
        connection.execute_batch(
            "CREATE TABLE IF NOT EXISTS agent_metadata (
                key TEXT PRIMARY KEY NOT NULL,
                value BLOB NOT NULL,
                CHECK (length(key) BETWEEN 1 AND 64),
                CHECK (length(value) <= 1048576)
             ) STRICT;
             CREATE TABLE IF NOT EXISTS command_journal (
                idempotency_key BLOB PRIMARY KEY NOT NULL,
                command_id BLOB NOT NULL,
                payload_sha256 BLOB NOT NULL,
                payload_hash_version INTEGER NOT NULL DEFAULT 0 CHECK (payload_hash_version >= 0),
                state TEXT NOT NULL CHECK (state IN ('accepted','running','succeeded','failed','unknown')),
                result BLOB,
                error_code TEXT CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 128),
                privileged_result_proof BLOB CHECK (privileged_result_proof IS NULL OR length(privileged_result_proof) BETWEEN 1 AND 65536),
                accepted_at INTEGER NOT NULL,
                updated_at INTEGER NOT NULL,
                CHECK (length(idempotency_key) = 16),
                CHECK (length(command_id) = 16),
                CHECK (length(payload_sha256) = 32),
                CHECK (result IS NULL OR length(result) <= 1048576)
             ) STRICT;
             CREATE INDEX IF NOT EXISTS command_journal_updated_at
                ON command_journal(updated_at);
             CREATE UNIQUE INDEX IF NOT EXISTS command_journal_command_id_unique
                ON command_journal(command_id);
             CREATE TABLE IF NOT EXISTS synthetic_effects (
                idempotency_key BLOB PRIMARY KEY NOT NULL,
                payload_sha256 BLOB NOT NULL,
                result BLOB NOT NULL,
                executed_at INTEGER NOT NULL,
                CHECK (length(idempotency_key) = 16),
                CHECK (length(payload_sha256) = 32),
                CHECK (length(result) <= 1048576)
             ) STRICT;
             CREATE TABLE IF NOT EXISTS synthetic_effect_counter (
                singleton INTEGER PRIMARY KEY NOT NULL CHECK (singleton = 1),
                executions INTEGER NOT NULL CHECK (executions >= 0)
             ) STRICT;
             INSERT OR IGNORE INTO synthetic_effect_counter(singleton,executions) VALUES (1,0);
             CREATE TABLE IF NOT EXISTS telemetry_buffer (
                batch_id BLOB PRIMARY KEY NOT NULL,
                priority INTEGER NOT NULL CHECK (priority BETWEEN 1 AND 4),
                observed_at INTEGER NOT NULL,
                expires_at INTEGER NOT NULL,
                payload BLOB NOT NULL,
                CHECK (length(batch_id) = 16),
                CHECK (length(payload) BETWEEN 1 AND 524288),
                CHECK (expires_at > observed_at)
             ) STRICT;
             CREATE INDEX IF NOT EXISTS telemetry_buffer_eviction
                ON telemetry_buffer(priority DESC, observed_at ASC);
             CREATE TABLE IF NOT EXISTS telemetry_drop_counters (
                priority INTEGER PRIMARY KEY NOT NULL CHECK (priority BETWEEN 1 AND 4),
                dropped INTEGER NOT NULL DEFAULT 0 CHECK (dropped >= 0)
             ) STRICT;
             CREATE TABLE IF NOT EXISTS applied_resource_revisions (
                resource_type TEXT NOT NULL CHECK (resource_type IN ('user','group','config')),
                resource_key TEXT NOT NULL CHECK (length(resource_key) BETWEEN 1 AND 64),
                revision INTEGER NOT NULL CHECK (revision > 0),
                updated_at INTEGER NOT NULL,
                PRIMARY KEY(resource_type,resource_key)
             ) STRICT;",
        )?;
        migrate_command_journal(&connection)?;
        migrate_applied_resource_revisions(&connection)?;
        let integrity: String =
            connection.pragma_query_value(None, "quick_check", |row| row.get(0))?;
        if integrity != "ok" {
            return Err(rusqlite::Error::InvalidQuery);
        }
        Ok(Self { connection })
    }

    /// Persists `Accepted` before execution, or returns the existing record for a replay.
    ///
    /// # Errors
    ///
    /// Returns a `SQLite` error without allowing the caller to execute a side effect.
    pub fn accept_command(
        &mut self,
        idempotency_key: &[u8; 16],
        command_id: &[u8; 16],
        payload_sha256: &[u8; 32],
        payload_hash_version: i32,
        now: i64,
    ) -> Result<AcceptOutcome, rusqlite::Error> {
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let by_key = query_command(&transaction, idempotency_key)?;
        let by_command = query_command_by_id(&transaction, command_id)?;
        let outcome = match (by_key, by_command) {
            (None, None) => {
                transaction.execute(
                    "INSERT INTO command_journal(idempotency_key,command_id,payload_sha256,payload_hash_version,state,result,error_code,privileged_result_proof,accepted_at,updated_at) VALUES (?1,?2,?3,?4,'accepted',NULL,NULL,NULL,?5,?5)",
                    rusqlite::params![idempotency_key.as_slice(), command_id.as_slice(), payload_sha256.as_slice(), payload_hash_version, now],
                )?;
                AcceptOutcome::Accepted(
                    query_command(&transaction, idempotency_key)?
                        .ok_or(rusqlite::Error::QueryReturnedNoRows)?,
                )
            }
            (Some(by_key), Some(by_command))
                if by_key.idempotency_key == by_command.idempotency_key
                    && by_key.command_id == *command_id =>
            {
                if by_key.payload_sha256 == *payload_sha256
                    && by_key.payload_hash_version == payload_hash_version
                {
                    AcceptOutcome::Replay(by_key)
                } else {
                    AcceptOutcome::PayloadConflict(by_key)
                }
            }
            (Some(existing), _) | (_, Some(existing)) => AcceptOutcome::IdentityConflict(existing),
        };
        transaction.commit()?;
        Ok(outcome)
    }

    /// Loads a command record by idempotency key.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn command(
        &self,
        idempotency_key: &[u8; 16],
    ) -> Result<Option<CommandRecord>, rusqlite::Error> {
        query_command(&self.connection, idempotency_key)
    }

    /// Loads a command record by command identity.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn command_by_id(
        &self,
        command_id: &[u8; 16],
    ) -> Result<Option<CommandRecord>, rusqlite::Error> {
        query_command_by_id(&self.connection, command_id)
    }

    /// Moves a command to a new state and durably stores its bounded result.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid transitions, oversized fields, or `SQLite` failure.
    pub fn transition_command(
        &self,
        idempotency_key: &[u8; 16],
        from: &[CommandState],
        to: CommandState,
        result: Option<&[u8]>,
        error_code: Option<&str>,
        now: i64,
    ) -> Result<CommandRecord, rusqlite::Error> {
        if from.is_empty()
            || result.is_some_and(|value| value.len() > 1024 * 1024)
            || error_code.is_some_and(|value| value.is_empty() || value.len() > 128)
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let current = self
            .command(idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        if !from.contains(&current.state) {
            return Err(rusqlite::Error::InvalidQuery);
        }
        self.connection.execute(
            "UPDATE command_journal SET state=?2,result=?3,error_code=?4,updated_at=?5 WHERE idempotency_key=?1 AND state=?6",
            rusqlite::params![idempotency_key.as_slice(), to.as_str(), result, error_code, now, current.state.as_str()],
        )?;
        self.command(idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)
    }

    /// Atomically persists a terminal result together with the exact privd proof.
    ///
    /// # Errors
    ///
    /// Rejects missing or oversized proof and every invalid state transition.
    #[allow(clippy::too_many_arguments)]
    pub fn transition_command_attested(
        &self,
        idempotency_key: &[u8; 16],
        from: &[CommandState],
        to: CommandState,
        result: Option<&[u8]>,
        error_code: Option<&str>,
        proof: &[u8],
        now: i64,
    ) -> Result<CommandRecord, rusqlite::Error> {
        if from.is_empty()
            || !matches!(to, CommandState::Succeeded | CommandState::Failed)
            || result.is_some_and(|value| value.len() > 1024 * 1024)
            || error_code.is_some_and(|value| value.is_empty() || value.len() > 128)
            || proof.is_empty()
            || proof.len() > 65_536
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let current = self
            .command(idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        if !from.contains(&current.state) {
            return Err(rusqlite::Error::InvalidQuery);
        }
        self.connection.execute(
            "UPDATE command_journal SET state=?2,result=?3,error_code=?4,privileged_result_proof=?5,updated_at=?6 WHERE idempotency_key=?1 AND state=?7",
            rusqlite::params![idempotency_key.as_slice(), to.as_str(), result, error_code, proof, now, current.state.as_str()],
        )?;
        self.command(idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)
    }

    /// Atomically records a successful external command and its applied resource revision.
    ///
    /// # Errors
    ///
    /// Rejects invalid identity, resource metadata, revision regression, or `SQLite` failure.
    pub fn complete_external_with_revision(
        &mut self,
        idempotency_key: &[u8; 16],
        command_id: &[u8; 16],
        resource: AppliedResourceRevision<'_>,
        result: &[u8],
        now: i64,
    ) -> Result<CommandRecord, rusqlite::Error> {
        if !matches!(resource.resource_type, "user" | "group" | "config")
            || resource.resource_key.is_empty()
            || resource.resource_key.len() > 64
            || resource.revision == 0
            || resource.revision > i64::MAX as u64
            || result.len() > 1024 * 1024
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let record = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        if record.command_id != *command_id || record.state != CommandState::Running {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let revision =
            i64::try_from(resource.revision).map_err(|_| rusqlite::Error::InvalidQuery)?;
        transaction.execute(
            "INSERT INTO applied_resource_revisions(resource_type,resource_key,revision,updated_at) VALUES(?1,?2,?3,?4) ON CONFLICT(resource_type,resource_key) DO UPDATE SET revision=excluded.revision,updated_at=excluded.updated_at WHERE excluded.revision >= applied_resource_revisions.revision",
            rusqlite::params![resource.resource_type, resource.resource_key, revision, now],
        )?;
        let stored: i64 = transaction.query_row(
            "SELECT revision FROM applied_resource_revisions WHERE resource_type=?1 AND resource_key=?2",
            rusqlite::params![resource.resource_type, resource.resource_key],
            |row| row.get(0),
        )?;
        if stored != revision {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let updated = transaction.execute(
            "UPDATE command_journal SET state='succeeded',result=?3,error_code=NULL,updated_at=?4 WHERE idempotency_key=?1 AND command_id=?2 AND state='running'",
            rusqlite::params![idempotency_key.as_slice(), command_id.as_slice(), result, now],
        )?;
        if updated != 1 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let completed = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        transaction.commit()?;
        Ok(completed)
    }

    /// Atomically stores an applied revision, terminal result, and privd proof.
    ///
    /// # Errors
    ///
    /// Rejects invalid identity, revision regression, or receipt persistence failure.
    pub fn complete_external_with_revision_attested(
        &mut self,
        idempotency_key: &[u8; 16],
        command_id: &[u8; 16],
        resource: AppliedResourceRevision<'_>,
        result: &[u8],
        proof: &[u8],
        now: i64,
    ) -> Result<CommandRecord, rusqlite::Error> {
        if !matches!(resource.resource_type, "user" | "group" | "config")
            || resource.resource_key.is_empty()
            || resource.resource_key.len() > 64
            || resource.revision == 0
            || resource.revision > i64::MAX as u64
            || result.len() > 1024 * 1024
            || proof.is_empty()
            || proof.len() > 65_536
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let record = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        if record.command_id != *command_id || record.state != CommandState::Running {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let revision =
            i64::try_from(resource.revision).map_err(|_| rusqlite::Error::InvalidQuery)?;
        transaction.execute(
            "INSERT INTO applied_resource_revisions(resource_type,resource_key,revision,updated_at) VALUES(?1,?2,?3,?4) ON CONFLICT(resource_type,resource_key) DO UPDATE SET revision=excluded.revision,updated_at=excluded.updated_at WHERE excluded.revision >= applied_resource_revisions.revision",
            rusqlite::params![resource.resource_type, resource.resource_key, revision, now],
        )?;
        let stored: i64 = transaction.query_row(
            "SELECT revision FROM applied_resource_revisions WHERE resource_type=?1 AND resource_key=?2",
            rusqlite::params![resource.resource_type, resource.resource_key],
            |row| row.get(0),
        )?;
        if stored != revision {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let updated = transaction.execute(
            "UPDATE command_journal SET state='succeeded',result=?3,error_code=NULL,privileged_result_proof=?4,updated_at=?5 WHERE idempotency_key=?1 AND command_id=?2 AND state='running'",
            rusqlite::params![idempotency_key.as_slice(), command_id.as_slice(), result, proof, now],
        )?;
        if updated != 1 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let completed = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        transaction.commit()?;
        Ok(completed)
    }

    /// Atomically recovers an observed desired-state effect and its applied revision.
    ///
    /// # Errors
    ///
    /// Rejects terminal failures, identity mismatches, revision regressions, or `SQLite` failure.
    pub fn reconcile_external_with_revision(
        &mut self,
        idempotency_key: &[u8; 16],
        command_id: &[u8; 16],
        resource: AppliedResourceRevision<'_>,
        result: &[u8],
        now: i64,
    ) -> Result<CommandRecord, rusqlite::Error> {
        if !matches!(resource.resource_type, "user" | "group" | "config")
            || resource.resource_key.is_empty()
            || resource.resource_key.len() > 64
            || resource.revision == 0
            || resource.revision > i64::MAX as u64
            || result.len() > 1024 * 1024
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let record = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        if record.command_id != *command_id
            || !matches!(
                record.state,
                CommandState::Accepted | CommandState::Running | CommandState::Unknown
            )
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let revision =
            i64::try_from(resource.revision).map_err(|_| rusqlite::Error::InvalidQuery)?;
        transaction.execute(
            "INSERT INTO applied_resource_revisions(resource_type,resource_key,revision,updated_at) VALUES(?1,?2,?3,?4) ON CONFLICT(resource_type,resource_key) DO UPDATE SET revision=excluded.revision,updated_at=excluded.updated_at WHERE excluded.revision >= applied_resource_revisions.revision",
            rusqlite::params![resource.resource_type, resource.resource_key, revision, now],
        )?;
        let stored: i64 = transaction.query_row(
            "SELECT revision FROM applied_resource_revisions WHERE resource_type=?1 AND resource_key=?2",
            rusqlite::params![resource.resource_type, resource.resource_key],
            |row| row.get(0),
        )?;
        if stored != revision {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let updated = transaction.execute(
            "UPDATE command_journal SET state='succeeded',result=?3,error_code=NULL,updated_at=?4 WHERE idempotency_key=?1 AND command_id=?2 AND state IN ('accepted','running','unknown')",
            rusqlite::params![idempotency_key.as_slice(), command_id.as_slice(), result, now],
        )?;
        if updated != 1 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let completed = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        transaction.commit()?;
        Ok(completed)
    }

    /// Returns the last durably applied revision for one local resource.
    ///
    /// # Errors
    ///
    /// Returns a `SQLite` query error.
    pub fn applied_revision(
        &self,
        resource_type: &str,
        resource_key: &str,
    ) -> Result<Option<u64>, rusqlite::Error> {
        let result = self.connection.query_row(
            "SELECT revision FROM applied_resource_revisions WHERE resource_type=?1 AND resource_key=?2",
            rusqlite::params![resource_type, resource_key],
            |row| row.get::<_, i64>(0),
        );
        match result {
            Ok(value) => u64::try_from(value)
                .map(Some)
                .map_err(|_| rusqlite::Error::InvalidQuery),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(error) => Err(error),
        }
    }

    /// Lists durable applied revisions for one resource type in key order.
    ///
    /// # Errors
    ///
    /// Returns a `SQLite` query error.
    pub fn applied_revisions(
        &self,
        resource_type: &str,
    ) -> Result<Vec<(String, u64)>, rusqlite::Error> {
        if !matches!(resource_type, "user" | "group" | "config") {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let mut statement = self.connection.prepare(
            "SELECT resource_key,revision FROM applied_resource_revisions WHERE resource_type=?1 ORDER BY resource_key",
        )?;
        let rows = statement.query_map([resource_type], |row| {
            let key: String = row.get(0)?;
            let revision: i64 = row.get(1)?;
            Ok((key, revision))
        })?;
        rows.map(|row| {
            let (key, revision) = row?;
            Ok((
                key,
                u64::try_from(revision).map_err(|_| rusqlite::Error::InvalidQuery)?,
            ))
        })
        .collect()
    }

    /// Commits the synthetic side effect and its counter atomically.
    ///
    /// Returns `true` only for the first execution. A conflicting payload is rejected.
    ///
    /// # Errors
    ///
    /// Returns a `SQLite` error; the surrounding transaction prevents a partial effect.
    pub fn execute_synthetic_effect(
        &mut self,
        idempotency_key: &[u8; 16],
        payload_sha256: &[u8; 32],
        result: &[u8],
        now: i64,
    ) -> Result<bool, rusqlite::Error> {
        if result.len() > 1024 * 1024 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let transaction = self.connection.transaction()?;
        if let Some(existing) = query_effect(&transaction, idempotency_key)? {
            if existing.payload_sha256 != *payload_sha256 {
                return Err(rusqlite::Error::InvalidQuery);
            }
            transaction.commit()?;
            return Ok(false);
        }
        transaction.execute(
            "INSERT INTO synthetic_effects(idempotency_key,payload_sha256,result,executed_at) VALUES (?1,?2,?3,?4)",
            rusqlite::params![idempotency_key.as_slice(), payload_sha256.as_slice(), result, now],
        )?;
        transaction.execute(
            "UPDATE synthetic_effect_counter SET executions=executions+1 WHERE singleton=1",
            [],
        )?;
        transaction.commit()?;
        Ok(true)
    }

    /// Commits the synthetic effect and terminal journal result atomically.
    ///
    /// # Errors
    ///
    /// Returns an error without committing either the effect or terminal result.
    pub fn execute_and_complete_synthetic(
        &mut self,
        idempotency_key: &[u8; 16],
        command_id: &[u8; 16],
        payload_sha256: &[u8; 32],
        result: &[u8],
        now: i64,
    ) -> Result<CommandRecord, rusqlite::Error> {
        if result.len() > 1024 * 1024 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let record = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        if record.command_id != *command_id
            || record.payload_sha256 != *payload_sha256
            || record.state != CommandState::Running
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        if let Some(existing) = query_effect(&transaction, idempotency_key)? {
            if existing.payload_sha256 != *payload_sha256 || existing.result != result {
                return Err(rusqlite::Error::InvalidQuery);
            }
        } else {
            transaction.execute(
                "INSERT INTO synthetic_effects(idempotency_key,payload_sha256,result,executed_at) VALUES (?1,?2,?3,?4)",
                rusqlite::params![idempotency_key.as_slice(), payload_sha256.as_slice(), result, now],
            )?;
            transaction.execute(
                "UPDATE synthetic_effect_counter SET executions=executions+1 WHERE singleton=1",
                [],
            )?;
        }
        let updated = transaction.execute(
            "UPDATE command_journal SET state='succeeded',result=?4,error_code=NULL,updated_at=?5 WHERE idempotency_key=?1 AND command_id=?2 AND payload_sha256=?3 AND state='running'",
            rusqlite::params![idempotency_key.as_slice(), command_id.as_slice(), payload_sha256.as_slice(), result, now],
        )?;
        if updated != 1 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let completed = query_command(&transaction, idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)?;
        transaction.commit()?;
        Ok(completed)
    }

    /// Observes a durable synthetic effect during Unknown reconciliation.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn synthetic_effect(
        &self,
        idempotency_key: &[u8; 16],
    ) -> Result<Option<SyntheticEffect>, rusqlite::Error> {
        query_effect(&self.connection, idempotency_key)
    }

    /// Returns the number of committed synthetic side effects.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn synthetic_execution_count(&self) -> Result<u64, rusqlite::Error> {
        self.connection.query_row(
            "SELECT executions FROM synthetic_effect_counter WHERE singleton=1",
            [],
            |row| row.get(0),
        )
    }

    /// Returns the active `SQLite` journal mode.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn journal_mode(&self) -> Result<String, rusqlite::Error> {
        self.connection
            .pragma_query_value(None, "journal_mode", |row| row.get(0))
    }

    /// Returns the active `SQLite` synchronous durability level.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn synchronous_level(&self) -> Result<u8, rusqlite::Error> {
        self.connection
            .pragma_query_value(None, "synchronous", |row| row.get(0))
    }

    /// Returns whether foreign keys are enabled.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn foreign_keys_enabled(&self) -> Result<bool, rusqlite::Error> {
        self.connection
            .pragma_query_value(None, "foreign_keys", |row| row.get(0))
    }

    /// Returns the configured busy timeout in milliseconds.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn busy_timeout_ms(&self) -> Result<u64, rusqlite::Error> {
        self.connection
            .pragma_query_value(None, "busy_timeout", |row| row.get(0))
    }

    /// Runs `SQLite`'s complete consistency check on the active journal connection.
    ///
    /// # Errors
    ///
    /// Returns a query error or `InvalidQuery` when `SQLite` reports corruption.
    pub fn integrity_check(&self) -> Result<(), rusqlite::Error> {
        let result: String =
            self.connection
                .pragma_query_value(None, "integrity_check", |row| row.get(0))?;
        if result == "ok" {
            Ok(())
        } else {
            Err(rusqlite::Error::InvalidQuery)
        }
    }

    /// Enqueues one bounded telemetry batch, evicting oldest low-priority data first.
    ///
    /// Returns `false` when this batch was itself evicted. Expired records count as drops.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid limits, invalid input, or a `SQLite` failure.
    pub fn enqueue_telemetry(
        &mut self,
        insert: &TelemetryInsert<'_>,
    ) -> Result<bool, rusqlite::Error> {
        let TelemetryInsert {
            batch_id,
            priority,
            observed_at,
            expires_at,
            payload,
            now,
            max_bytes,
        } = *insert;
        if !(1..=4).contains(&priority)
            || payload.is_empty()
            || payload.len() > 512 * 1024
            || expires_at <= observed_at
            || max_bytes == 0
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let transaction = self.connection.transaction()?;
        prune_expired_telemetry(&transaction, now)?;
        transaction.execute(
            "INSERT INTO telemetry_buffer (batch_id,priority,observed_at,expires_at,payload) VALUES (?1,?2,?3,?4,?5) ON CONFLICT(batch_id) DO NOTHING",
            rusqlite::params![batch_id.as_slice(), priority, observed_at, expires_at, payload],
        )?;
        loop {
            let bytes: u64 = transaction.query_row(
                "SELECT coalesce(sum(length(payload)),0) FROM telemetry_buffer",
                [],
                |row| row.get(0),
            )?;
            if bytes <= max_bytes {
                break;
            }
            let evicted = transaction.query_row(
                "DELETE FROM telemetry_buffer WHERE batch_id=(SELECT batch_id FROM telemetry_buffer ORDER BY priority DESC,observed_at ASC,batch_id LIMIT 1) RETURNING batch_id,priority",
                [],
                |row| Ok((row.get::<_, Vec<u8>>(0)?, row.get::<_, u8>(1)?)),
            )?;
            increment_drop(&transaction, evicted.1, 1)?;
        }
        let retained = transaction.query_row(
            "SELECT EXISTS(SELECT 1 FROM telemetry_buffer WHERE batch_id=?1)",
            [batch_id.as_slice()],
            |row| row.get(0),
        )?;
        transaction.commit()?;
        Ok(retained)
    }

    /// Prunes records past their stored expiry or maximum offline recovery age,
    /// then returns eligible payloads in delivery priority and observation order.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn telemetry_pending(
        &mut self,
        limit: usize,
        now: i64,
    ) -> Result<Vec<PendingTelemetry>, rusqlite::Error> {
        if limit == 0 || limit > 256 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let transaction = self.connection.transaction()?;
        prune_expired_telemetry(&transaction, now)?;
        let mut statement = transaction.prepare(
            "SELECT batch_id,payload FROM telemetry_buffer ORDER BY priority,observed_at,batch_id LIMIT ?1",
        )?;
        let pending = statement
            .query_map([limit], |row| {
                let id = row.get::<_, Vec<u8>>(0)?;
                let id: [u8; 16] = id.try_into().map_err(|_| rusqlite::Error::InvalidQuery)?;
                Ok((id, row.get(1)?))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        drop(statement);
        transaction.commit()?;
        Ok(pending)
    }

    /// Removes a delivered batch idempotently.
    ///
    /// # Errors
    ///
    /// Returns a `SQLite` failure.
    pub fn acknowledge_telemetry(&self, batch_id: &[u8; 16]) -> Result<(), rusqlite::Error> {
        self.connection.execute(
            "DELETE FROM telemetry_buffer WHERE batch_id=?1",
            [batch_id.as_slice()],
        )?;
        Ok(())
    }

    /// Returns drop counters ordered as security, health, aggregate, raw history.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn telemetry_drop_counters(&self) -> Result<[u64; 4], rusqlite::Error> {
        let mut counters = [0_u64; 4];
        let mut statement = self
            .connection
            .prepare("SELECT priority,dropped FROM telemetry_drop_counters")?;
        let rows = statement.query_map([], |row| {
            Ok((row.get::<_, usize>(0)?, row.get::<_, u64>(1)?))
        })?;
        for row in rows {
            let (priority, count) = row?;
            counters[priority - 1] = count;
        }
        Ok(counters)
    }

    /// Reads the durable connection-owner fencing epoch floor, or zero when
    /// no fence has been accepted yet.
    ///
    /// # Errors
    ///
    /// Returns a query or decoding error.
    pub fn owner_fence_epoch_floor(&self) -> Result<u64, rusqlite::Error> {
        let value: Option<Vec<u8>> = self
            .connection
            .query_row(
                "SELECT value FROM agent_metadata WHERE key=?1",
                [OWNER_FENCE_EPOCH_FLOOR_KEY],
                |row| row.get(0),
            )
            .optional()?;
        let Some(bytes) = value else {
            return Ok(0);
        };
        bytes
            .try_into()
            .map(u64::from_be_bytes)
            .map_err(|_| rusqlite::Error::InvalidQuery)
    }

    /// Raises the durable fencing epoch floor. The stored value only ever
    /// increases, including across crashes, so a restarted Agent cannot be
    /// reset to accept an already-superseded owner epoch.
    ///
    /// # Errors
    ///
    /// Returns a write error.
    pub fn raise_owner_fence_epoch_floor(&self, epoch: u64) -> Result<(), rusqlite::Error> {
        let value = epoch.to_be_bytes();
        self.connection.execute(
            "INSERT INTO agent_metadata(key,value) VALUES(?1,?2)
             ON CONFLICT(key) DO UPDATE SET value=excluded.value
             WHERE excluded.value>agent_metadata.value",
            rusqlite::params![OWNER_FENCE_EPOCH_FLOOR_KEY, value.as_slice()],
        )?;
        Ok(())
    }
}

/// Applies idempotent column migrations to `command_journal`.
///
/// New databases already include every column via the `CREATE TABLE` statement.
/// Pre-existing databases are upgraded with `ALTER TABLE … ADD COLUMN` guarded
/// by a `PRAGMA table_info` presence check.
fn migrate_command_journal(connection: &Connection) -> Result<(), rusqlite::Error> {
    if !has_column(connection, "error_code")? {
        connection.execute(
            "ALTER TABLE command_journal ADD COLUMN error_code TEXT CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 128)",
            [],
        )?;
    }
    if !has_column(connection, "payload_hash_version")? {
        connection.execute(
            "ALTER TABLE command_journal ADD COLUMN payload_hash_version INTEGER NOT NULL DEFAULT 0 CHECK (payload_hash_version >= 0)",
            [],
        )?;
    }
    if !has_column(connection, "privileged_result_proof")? {
        connection.execute(
            "ALTER TABLE command_journal ADD COLUMN privileged_result_proof BLOB CHECK (privileged_result_proof IS NULL OR length(privileged_result_proof) BETWEEN 1 AND 65536)",
            [],
        )?;
    }
    Ok(())
}

fn migrate_applied_resource_revisions(connection: &Connection) -> Result<(), rusqlite::Error> {
    let schema: String = connection.query_row(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name='applied_resource_revisions'",
        [],
        |row| row.get(0),
    )?;
    if schema.contains("'config'") {
        return Ok(());
    }
    connection.execute_batch(
        "BEGIN IMMEDIATE;
         ALTER TABLE applied_resource_revisions RENAME TO applied_resource_revisions_legacy;
         CREATE TABLE applied_resource_revisions (
            resource_type TEXT NOT NULL CHECK (resource_type IN ('user','group','config')),
            resource_key TEXT NOT NULL CHECK (length(resource_key) BETWEEN 1 AND 64),
            revision INTEGER NOT NULL CHECK (revision > 0),
            updated_at INTEGER NOT NULL,
            PRIMARY KEY(resource_type,resource_key)
         ) STRICT;
         INSERT INTO applied_resource_revisions(resource_type,resource_key,revision,updated_at)
            SELECT resource_type,resource_key,revision,updated_at
            FROM applied_resource_revisions_legacy;
         DROP TABLE applied_resource_revisions_legacy;
         COMMIT;",
    )
}

/// Returns `true` when `command_journal` has a column named `column`.
fn has_column(connection: &Connection, column: &str) -> Result<bool, rusqlite::Error> {
    let mut statement = connection.prepare("PRAGMA table_info(command_journal)")?;
    let names = statement
        .query_map([], |row| row.get::<_, String>(1))?
        .collect::<Result<Vec<_>, _>>()?;
    Ok(names.iter().any(|name| name == column))
}

fn query_command(
    connection: &Connection,
    idempotency_key: &[u8; 16],
) -> Result<Option<CommandRecord>, rusqlite::Error> {
    let mut statement = connection.prepare(
        "SELECT idempotency_key,command_id,payload_sha256,payload_hash_version,state,result,error_code,privileged_result_proof,accepted_at,updated_at FROM command_journal WHERE idempotency_key=?1",
    )?;
    let mut rows = statement.query([idempotency_key.as_slice()])?;
    let Some(row) = rows.next()? else {
        return Ok(None);
    };
    let key = fixed_blob::<16>(row.get(0)?)?;
    let command_id = fixed_blob::<16>(row.get(1)?)?;
    let payload_sha256 = fixed_blob::<32>(row.get(2)?)?;
    Ok(Some(CommandRecord {
        idempotency_key: key,
        command_id,
        payload_sha256,
        payload_hash_version: row.get(3)?,
        state: CommandState::parse(&row.get::<_, String>(4)?)?,
        result: row.get(5)?,
        error_code: row.get(6)?,
        privileged_result_proof: row.get(7)?,
        accepted_at: row.get(8)?,
        updated_at: row.get(9)?,
    }))
}

fn query_command_by_id(
    connection: &Connection,
    command_id: &[u8; 16],
) -> Result<Option<CommandRecord>, rusqlite::Error> {
    let mut statement = connection.prepare(
        "SELECT idempotency_key,command_id,payload_sha256,payload_hash_version,state,result,error_code,privileged_result_proof,accepted_at,updated_at FROM command_journal WHERE command_id=?1",
    )?;
    let mut rows = statement.query([command_id.as_slice()])?;
    let Some(row) = rows.next()? else {
        return Ok(None);
    };
    Ok(Some(CommandRecord {
        idempotency_key: fixed_blob::<16>(row.get(0)?)?,
        command_id: fixed_blob::<16>(row.get(1)?)?,
        payload_sha256: fixed_blob::<32>(row.get(2)?)?,
        payload_hash_version: row.get(3)?,
        state: CommandState::parse(&row.get::<_, String>(4)?)?,
        result: row.get(5)?,
        error_code: row.get(6)?,
        privileged_result_proof: row.get(7)?,
        accepted_at: row.get(8)?,
        updated_at: row.get(9)?,
    }))
}

fn query_effect(
    connection: &Connection,
    idempotency_key: &[u8; 16],
) -> Result<Option<SyntheticEffect>, rusqlite::Error> {
    let mut statement = connection.prepare(
        "SELECT payload_sha256,result,executed_at FROM synthetic_effects WHERE idempotency_key=?1",
    )?;
    let mut rows = statement.query([idempotency_key.as_slice()])?;
    let Some(row) = rows.next()? else {
        return Ok(None);
    };
    Ok(Some(SyntheticEffect {
        payload_sha256: fixed_blob::<32>(row.get(0)?)?,
        result: row.get(1)?,
        executed_at: row.get(2)?,
    }))
}

fn fixed_blob<const N: usize>(value: Vec<u8>) -> Result<[u8; N], rusqlite::Error> {
    value.try_into().map_err(|_| rusqlite::Error::InvalidQuery)
}

fn increment_drop(
    transaction: &rusqlite::Transaction<'_>,
    priority: u8,
    count: u64,
) -> Result<(), rusqlite::Error> {
    transaction.execute("INSERT INTO telemetry_drop_counters(priority,dropped) VALUES (?1,?2) ON CONFLICT(priority) DO UPDATE SET dropped=dropped+excluded.dropped",rusqlite::params![priority,count])?;
    Ok(())
}

fn prune_expired_telemetry(
    transaction: &rusqlite::Transaction<'_>,
    now: i64,
) -> Result<(), rusqlite::Error> {
    let recovery_seconds = i64::try_from(OFFLINE_RECOVERY_RETENTION_SECONDS)
        .map_err(|_| rusqlite::Error::InvalidQuery)?;
    let observed_cutoff = now.saturating_sub(recovery_seconds);
    let mut expired = transaction.prepare(
        "SELECT priority, count(*) FROM telemetry_buffer WHERE expires_at <= ?1 OR observed_at <= ?2 GROUP BY priority",
    )?;
    let expired_counts = expired
        .query_map(rusqlite::params![now, observed_cutoff], |row| {
            Ok((row.get::<_, u8>(0)?, row.get::<_, u64>(1)?))
        })?
        .collect::<Result<Vec<_>, _>>()?;
    drop(expired);
    transaction.execute(
        "DELETE FROM telemetry_buffer WHERE expires_at <= ?1 OR observed_at <= ?2",
        rusqlite::params![now, observed_cutoff],
    )?;
    for (expired_priority, count) in expired_counts {
        increment_drop(transaction, expired_priority, count)?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt as _;

    use super::*;

    fn temporary_path(label: &str) -> std::path::PathBuf {
        std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("ocservia-{label}-{}.db", uuid::Uuid::now_v7()))
    }

    fn cleanup(path: &Path) {
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
        }
    }

    #[test]
    fn configures_wal_foreign_keys_and_busy_timeout() {
        let path = std::env::temp_dir()
            .canonicalize()
            .expect("canonical temporary directory")
            .join(format!("ocservia-journal-{}.db", uuid::Uuid::now_v7()));
        let journal = Journal::open(&path).expect("open journal");
        assert_eq!(
            journal
                .journal_mode()
                .expect("journal mode")
                .to_ascii_lowercase(),
            "wal"
        );
        assert!(journal.foreign_keys_enabled().expect("foreign keys"));
        assert_eq!(journal.busy_timeout_ms().expect("busy timeout"), 5000);
        assert_eq!(journal.synchronous_level().expect("synchronous"), 2);
        assert_eq!(
            std::fs::metadata(&path)
                .expect("journal metadata")
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
        drop(journal);
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
        }
    }

    #[test]
    fn terminal_result_and_privd_proof_commit_and_replay_together() {
        let path = temporary_path("attested-result");
        let mut journal = Journal::open(&path).expect("open");
        let key = *uuid::Uuid::now_v7().as_bytes();
        let command = *uuid::Uuid::now_v7().as_bytes();
        assert!(matches!(
            journal
                .accept_command(&key, &command, &[7; 32], 2, 10)
                .expect("accept"),
            AcceptOutcome::Accepted(_)
        ));
        journal
            .transition_command(
                &key,
                &[CommandState::Accepted],
                CommandState::Running,
                None,
                None,
                11,
            )
            .expect("run");
        let completed = journal
            .transition_command_attested(
                &key,
                &[CommandState::Running],
                CommandState::Succeeded,
                Some(b"exact-result"),
                None,
                b"opaque-root-receipt",
                12,
            )
            .expect("persist result and proof");
        assert_eq!(
            completed.result.as_deref(),
            Some(b"exact-result".as_slice())
        );
        assert_eq!(
            completed.privileged_result_proof.as_deref(),
            Some(b"opaque-root-receipt".as_slice())
        );
        drop(journal);
        let mut journal = Journal::open(&path).expect("reopen");
        let replay = journal
            .accept_command(&key, &command, &[7; 32], 2, 13)
            .expect("replay");
        let AcceptOutcome::Replay(record) = replay else {
            panic!("expected exact replay");
        };
        assert_eq!(record.result, completed.result);
        assert_eq!(
            record.privileged_result_proof,
            completed.privileged_result_proof
        );
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn telemetry_buffer_evicts_raw_before_security_and_counts_drops() {
        let path = std::env::temp_dir()
            .canonicalize()
            .expect("tmp")
            .join(format!("ocservia-telemetry-{}.db", uuid::Uuid::now_v7()));
        let mut journal = Journal::open(&path).expect("open");
        let security = *uuid::Uuid::now_v7().as_bytes();
        let raw = *uuid::Uuid::now_v7().as_bytes();
        assert!(
            journal
                .enqueue_telemetry(&TelemetryInsert {
                    batch_id: &security,
                    priority: 1,
                    observed_at: 10,
                    expires_at: 100,
                    payload: b"security",
                    now: 20,
                    max_bytes: 12
                })
                .expect("security")
        );
        assert!(
            !journal
                .enqueue_telemetry(&TelemetryInsert {
                    batch_id: &raw,
                    priority: 4,
                    observed_at: 11,
                    expires_at: 100,
                    payload: b"raw-data",
                    now: 20,
                    max_bytes: 12
                })
                .expect("raw")
        );
        let pending = journal.telemetry_pending(10, 20).expect("pending");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].0, security);
        assert_eq!(
            journal.telemetry_drop_counters().expect("drops"),
            [0, 0, 0, 1]
        );
        journal.acknowledge_telemetry(&security).expect("ack");
        assert!(journal.telemetry_pending(10, 20).expect("empty").is_empty());
        drop(journal);
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
        }
    }

    #[test]
    fn five_minute_offline_recovery_caps_legacy_telemetry_at_boundary() {
        let recovery_seconds = i64::try_from(OFFLINE_RECOVERY_RETENTION_SECONDS).expect("seconds");
        let path = temporary_path("five-minute-offline-recovery");
        let mut journal = Journal::open(&path).expect("open");
        let legacy = *uuid::Uuid::now_v7().as_bytes();
        let recovered = *uuid::Uuid::now_v7().as_bytes();
        journal
            .enqueue_telemetry(&TelemetryInsert {
                batch_id: &legacy,
                priority: 2,
                observed_at: 1_000,
                expires_at: 1_000 + 86_400,
                payload: b"legacy-day-window",
                now: 1_000,
                max_bytes: 1_024,
            })
            .expect("legacy telemetry");
        drop(journal);

        let mut journal = Journal::open(&path).expect("reopen upgraded journal");
        assert!(
            journal
                .telemetry_pending(10, 1_299)
                .expect("pending before boundary")
                .iter()
                .any(|item| item.0 == legacy)
        );
        assert!(
            journal
                .telemetry_pending(10, 1_300)
                .expect("pending at boundary")
                .iter()
                .all(|item| item.0 != legacy)
        );
        assert_eq!(
            journal.telemetry_drop_counters().expect("boundary drop"),
            [0, 1, 0, 0]
        );

        journal
            .enqueue_telemetry(&TelemetryInsert {
                batch_id: &recovered,
                priority: 2,
                observed_at: 1_300,
                expires_at: 1_300 + recovery_seconds,
                payload: b"reconnected",
                now: 1_300,
                max_bytes: 1_024,
            })
            .expect("recovery telemetry");
        let pending = journal
            .telemetry_pending(10, 1_301)
            .expect("pending after recovery");
        assert!(!pending.iter().any(|item| item.0 == legacy));
        assert!(pending.iter().any(|item| item.0 == recovered));
        assert_eq!(
            journal.telemetry_drop_counters().expect("drops"),
            [0, 1, 0, 0]
        );
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn command_key_binds_payload_and_replays_persisted_result() {
        let path = temporary_path("commands");
        let mut journal = Journal::open(&path).expect("open");
        let key = *uuid::Uuid::now_v7().as_bytes();
        let command = *uuid::Uuid::now_v7().as_bytes();
        let payload = [7_u8; 32];
        assert!(matches!(
            journal
                .accept_command(&key, &command, &payload, 0, 10)
                .expect("accept"),
            AcceptOutcome::Accepted(_)
        ));
        let completed = journal
            .transition_command(
                &key,
                &[CommandState::Accepted],
                CommandState::Succeeded,
                Some(b"result"),
                None,
                11,
            )
            .expect("complete");
        assert_eq!(completed.result.as_deref(), Some(b"result".as_slice()));
        assert!(matches!(
            journal
                .accept_command(&key, &command, &payload, 0, 12)
                .expect("replay"),
            AcceptOutcome::Replay(CommandRecord {
                state: CommandState::Succeeded,
                ..
            })
        ));
        assert!(matches!(
            journal
                .accept_command(&key, &command, &[8; 32], 0, 13)
                .expect("conflict"),
            AcceptOutcome::PayloadConflict(_)
        ));
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn external_result_and_applied_revision_commit_together() {
        let path = temporary_path("applied-revision");
        let mut journal = Journal::open(&path).expect("open");
        let key = *uuid::Uuid::now_v7().as_bytes();
        let command = *uuid::Uuid::now_v7().as_bytes();
        let payload = [7_u8; 32];
        journal
            .accept_command(&key, &command, &payload, 1, 10)
            .expect("accept");
        journal
            .transition_command(
                &key,
                &[CommandState::Accepted],
                CommandState::Running,
                None,
                None,
                11,
            )
            .expect("running");
        journal
            .complete_external_with_revision(
                &key,
                &command,
                AppliedResourceRevision {
                    resource_type: "user",
                    resource_key: "alice",
                    revision: 2,
                },
                b"applied",
                12,
            )
            .expect("complete");
        assert_eq!(
            journal.applied_revision("user", "alice").expect("revision"),
            Some(2)
        );
        drop(journal);
        let journal = Journal::open(&path).expect("reopen");
        assert_eq!(
            journal.applied_revision("user", "alice").expect("durable"),
            Some(2)
        );
        assert_eq!(
            journal
                .command(&key)
                .expect("command")
                .expect("record")
                .state,
            CommandState::Succeeded
        );
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn legacy_resource_revision_schema_migrates_without_losing_fences() {
        let path = temporary_path("resource-revision-schema");
        let legacy = Connection::open(&path).expect("open legacy database");
        legacy
            .execute_batch(
                "CREATE TABLE applied_resource_revisions (
                    resource_type TEXT NOT NULL CHECK (resource_type IN ('user','group')),
                    resource_key TEXT NOT NULL CHECK (length(resource_key) BETWEEN 1 AND 64),
                    revision INTEGER NOT NULL CHECK (revision > 0),
                    updated_at INTEGER NOT NULL,
                    PRIMARY KEY(resource_type,resource_key)
                 ) STRICT;
                 INSERT INTO applied_resource_revisions VALUES ('user','alice',4,10);",
            )
            .expect("seed legacy revision fence");
        drop(legacy);

        let mut journal = Journal::open(&path).expect("migrate journal");
        assert_eq!(
            journal
                .applied_revision("user", "alice")
                .expect("preserved user revision"),
            Some(4)
        );
        let key = *uuid::Uuid::now_v7().as_bytes();
        let command = *uuid::Uuid::now_v7().as_bytes();
        journal
            .accept_command(&key, &command, &[9; 32], 1, 11)
            .expect("accept config command");
        journal
            .transition_command(
                &key,
                &[CommandState::Accepted],
                CommandState::Running,
                None,
                None,
                12,
            )
            .expect("run config command");
        journal
            .complete_external_with_revision(
                &key,
                &command,
                AppliedResourceRevision {
                    resource_type: "config",
                    resource_key: "ocserv.conf",
                    revision: 1,
                },
                b"applied",
                13,
            )
            .expect("persist config fence");
        assert_eq!(
            journal
                .applied_revision("config", "ocserv.conf")
                .expect("config revision"),
            Some(1)
        );
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn reconciled_external_result_and_revision_recover_atomically_after_commit_failure() {
        let path = temporary_path("reconciled-applied-revision");
        let mut journal = Journal::open(&path).expect("open");
        let key = *uuid::Uuid::now_v7().as_bytes();
        let command = *uuid::Uuid::now_v7().as_bytes();
        let payload = [8_u8; 32];
        journal
            .accept_command(&key, &command, &payload, 1, 10)
            .expect("accept");
        journal
            .transition_command(
                &key,
                &[CommandState::Accepted],
                CommandState::Unknown,
                None,
                Some("result_persistence_failed"),
                11,
            )
            .expect("unknown");
        journal
            .connection
            .execute_batch(
                "CREATE TEMP TRIGGER fail_reconciled_terminal_result
                 BEFORE UPDATE OF state ON command_journal
                 WHEN NEW.state = 'succeeded'
                 BEGIN SELECT RAISE(FAIL, 'injected reconciled write failure'); END;",
            )
            .expect("failure trigger");
        assert!(
            journal
                .reconcile_external_with_revision(
                    &key,
                    &command,
                    AppliedResourceRevision {
                        resource_type: "user",
                        resource_key: "alice",
                        revision: 7,
                    },
                    b"observed",
                    12,
                )
                .is_err()
        );
        assert_eq!(journal.applied_revision("user", "alice").unwrap(), None);
        assert_eq!(
            journal.command(&key).unwrap().unwrap().state,
            CommandState::Unknown
        );
        journal
            .connection
            .execute_batch("DROP TRIGGER fail_reconciled_terminal_result")
            .expect("drop trigger");
        let recovered = journal
            .reconcile_external_with_revision(
                &key,
                &command,
                AppliedResourceRevision {
                    resource_type: "user",
                    resource_key: "alice",
                    revision: 7,
                },
                b"observed",
                13,
            )
            .expect("recover");
        assert_eq!(recovered.state, CommandState::Succeeded);
        assert_eq!(journal.applied_revision("user", "alice").unwrap(), Some(7));
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn command_key_and_command_id_are_one_to_one() {
        let path = temporary_path("identity");
        let mut journal = Journal::open(&path).expect("open");
        let key = *uuid::Uuid::now_v7().as_bytes();
        let command = *uuid::Uuid::now_v7().as_bytes();
        let payload = [7_u8; 32];
        assert!(matches!(
            journal
                .accept_command(&key, &command, &payload, 0, 10)
                .expect("accept"),
            AcceptOutcome::Accepted(_)
        ));
        assert!(matches!(
            journal
                .accept_command(&key, &command, &payload, 0, 11)
                .expect("exact replay"),
            AcceptOutcome::Replay(_)
        ));
        assert!(matches!(
            journal
                .accept_command(&key, uuid::Uuid::now_v7().as_bytes(), &payload, 0, 12)
                .expect("same key conflict"),
            AcceptOutcome::IdentityConflict(_)
        ));
        assert!(matches!(
            journal
                .accept_command(uuid::Uuid::now_v7().as_bytes(), &command, &payload, 0, 13)
                .expect("same command conflict"),
            AcceptOutcome::IdentityConflict(_)
        ));
        assert!(matches!(
            journal
                .accept_command(&key, &command, &[8_u8; 32], 0, 14)
                .expect("payload conflict"),
            AcceptOutcome::PayloadConflict(_)
        ));
        assert_eq!(journal.synthetic_execution_count().expect("count"), 0);
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn synthetic_effect_and_terminal_result_roll_back_together() {
        let path = temporary_path("atomic-completion");
        let key = *uuid::Uuid::now_v7().as_bytes();
        let command = *uuid::Uuid::now_v7().as_bytes();
        let payload = [9_u8; 32];
        let mut journal = Journal::open(&path).expect("open");
        journal
            .accept_command(&key, &command, &payload, 0, 10)
            .expect("accept");
        journal
            .transition_command(
                &key,
                &[CommandState::Accepted],
                CommandState::Running,
                None,
                None,
                11,
            )
            .expect("running");
        journal
            .connection
            .execute_batch(
                "CREATE TEMP TRIGGER fail_terminal_result
                 BEFORE UPDATE OF state ON command_journal
                 WHEN NEW.state = 'succeeded'
                 BEGIN SELECT RAISE(FAIL, 'injected terminal write failure'); END;",
            )
            .expect("failure trigger");
        assert!(
            journal
                .execute_and_complete_synthetic(&key, &command, &payload, b"result", 12)
                .is_err()
        );
        assert!(
            journal
                .synthetic_effect(&key)
                .expect("effect query")
                .is_none()
        );
        assert_eq!(journal.synthetic_execution_count().expect("count"), 0);
        assert_eq!(
            journal.command(&key).expect("command query").unwrap().state,
            CommandState::Running
        );
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn synthetic_effect_is_atomic_and_counted_once_across_restart() {
        let path = temporary_path("effect");
        let key = *uuid::Uuid::now_v7().as_bytes();
        let payload = [3_u8; 32];
        {
            let mut journal = Journal::open(&path).expect("open");
            assert!(
                journal
                    .execute_synthetic_effect(&key, &payload, b"done", 10)
                    .expect("first")
            );
        }
        let mut journal = Journal::open(&path).expect("reopen");
        assert!(
            !journal
                .execute_synthetic_effect(&key, &payload, b"done", 11)
                .expect("duplicate")
        );
        assert_eq!(journal.synthetic_execution_count().expect("count"), 1);
        assert_eq!(
            journal
                .synthetic_effect(&key)
                .expect("effect")
                .expect("present")
                .result,
            b"done"
        );
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn corrupt_database_is_refused() {
        let path = temporary_path("corrupt");
        std::fs::write(&path, b"not a sqlite database").expect("write corrupt fixture");
        assert!(Journal::open(&path).is_err());
        cleanup(&path);
    }

    #[test]
    fn read_only_database_cannot_accept_a_command() {
        let path = temporary_path("readonly");
        drop(Journal::open(&path).expect("create"));
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o444))
            .expect("make read-only");
        let result = Journal::open(&path)
            .and_then(|mut journal| journal.accept_command(&[1; 16], &[2; 16], &[3; 32], 0, 10));
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))
            .expect("restore permissions");
        assert!(result.is_err());
        cleanup(&path);
    }

    #[test]
    fn full_database_does_not_commit_a_partial_effect() {
        let path = temporary_path("full");
        let key = [1_u8; 16];
        let payload = [2_u8; 32];
        let mut journal = Journal::open(&path).expect("open");
        journal
            .accept_command(&key, &[3; 16], &payload, 0, 10)
            .expect("accept");
        journal
            .connection
            .pragma_update(None, "wal_checkpoint", "TRUNCATE")
            .ok();
        let pages: u32 = journal
            .connection
            .pragma_query_value(None, "page_count", |row| row.get(0))
            .expect("pages");
        journal
            .connection
            .pragma_update(None, "max_page_count", pages)
            .expect("limit pages");
        assert!(
            journal
                .execute_synthetic_effect(&key, &payload, &vec![7; 900_000], 11)
                .is_err()
        );
        assert!(journal.synthetic_effect(&key).expect("query").is_none());
        assert_eq!(journal.synthetic_execution_count().expect("count"), 0);
        drop(journal);
        cleanup(&path);
    }

    #[test]
    fn owner_fence_epoch_floor_is_monotonic_and_durable() {
        let path = temporary_path("fence-floor");
        {
            let journal = Journal::open(&path).expect("open");
            assert_eq!(journal.owner_fence_epoch_floor().expect("floor"), 0);
            journal
                .raise_owner_fence_epoch_floor(4)
                .expect("raise first");
            journal
                .raise_owner_fence_epoch_floor(2)
                .expect("raise lower ignored");
            assert_eq!(journal.owner_fence_epoch_floor().expect("floor"), 4);
            journal
                .raise_owner_fence_epoch_floor(9)
                .expect("raise higher");
            assert_eq!(journal.owner_fence_epoch_floor().expect("floor"), 9);
        }
        let reopened = Journal::open(&path).expect("reopen");
        assert_eq!(reopened.owner_fence_epoch_floor().expect("floor"), 9);
        reopened
            .raise_owner_fence_epoch_floor(3)
            .expect("raise lower ignored after reopen");
        assert_eq!(reopened.owner_fence_epoch_floor().expect("floor"), 9);
        drop(reopened);
        cleanup(&path);
    }
}
