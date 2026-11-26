//! Durable bounded Agent state and command-journal foundation.

#![forbid(unsafe_code)]

mod telemetry;
#[cfg(test)]
#[allow(clippy::too_many_lines)]
mod tests;

use std::path::Path;
use std::time::Duration;
use std::{fs, os::unix::fs::PermissionsExt as _};

use rusqlite::{Connection, OpenFlags, OptionalExtension, TransactionBehavior};

pub use crate::telemetry::{OFFLINE_RECOVERY_RETENTION_SECONDS, PendingTelemetry, TelemetryInsert};

const BUSY_TIMEOUT: Duration = Duration::from_secs(5);
const OWNER_FENCE_EPOCH_FLOOR_KEY: &str = "owner_fence_epoch_floor";

/// SQLite-backed Agent local state.
#[derive(Debug)]
pub struct Journal {
    pub(crate) connection: Connection,
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
    /// Algorithm version that produced `payload_sha256` (0 = legacy, 1 = v1, 2 = v2 canonical).
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
             CREATE INDEX IF NOT EXISTS telemetry_buffer_expires_idx
                ON telemetry_buffer(expires_at);
             CREATE INDEX IF NOT EXISTS telemetry_buffer_observed_idx
                ON telemetry_buffer(observed_at);
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
                transaction
                    .prepare_cached(
                        "INSERT INTO command_journal(idempotency_key,command_id,payload_sha256,payload_hash_version,state,result,error_code,privileged_result_proof,accepted_at,updated_at) VALUES (?1,?2,?3,?4,'accepted',NULL,NULL,NULL,?5,?5)",
                    )?
                    .execute(rusqlite::params![
                        idempotency_key.as_slice(),
                        command_id.as_slice(),
                        payload_sha256.as_slice(),
                        payload_hash_version,
                        now
                    ])?;
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
    /// Transitions a command between persisted states while holding the `SQLite`
    /// `idempotency_key`+`state` guard.
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
        self.transition_command_inner(idempotency_key, from, to, result, error_code, None, now)
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
        if !matches!(to, CommandState::Succeeded | CommandState::Failed)
            || proof.is_empty()
            || proof.len() > 65_536
        {
            return Err(rusqlite::Error::InvalidQuery);
        }
        self.transition_command_inner(
            idempotency_key,
            from,
            to,
            result,
            error_code,
            Some(proof),
            now,
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn transition_command_inner(
        &self,
        idempotency_key: &[u8; 16],
        from: &[CommandState],
        to: CommandState,
        result: Option<&[u8]>,
        error_code: Option<&str>,
        proof: Option<&[u8]>,
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
        if let Some(proof) = proof {
            self.connection
                .prepare_cached(
                    "UPDATE command_journal SET state=?2,result=?3,error_code=?4,privileged_result_proof=?5,updated_at=?6 WHERE idempotency_key=?1 AND state=?7",
                )?
                .execute(rusqlite::params![
                    idempotency_key.as_slice(),
                    to.as_str(),
                    result,
                    error_code,
                    proof,
                    now,
                    current.state.as_str()
                ])?;
        } else {
            self.connection
                .prepare_cached(
                    "UPDATE command_journal SET state=?2,result=?3,error_code=?4,updated_at=?5 WHERE idempotency_key=?1 AND state=?6",
                )?
                .execute(rusqlite::params![
                    idempotency_key.as_slice(),
                    to.as_str(),
                    result,
                    error_code,
                    now,
                    current.state.as_str()
                ])?;
        }
        self.command(idempotency_key)?
            .ok_or(rusqlite::Error::QueryReturnedNoRows)
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
        transaction
            .prepare_cached(
                "INSERT INTO applied_resource_revisions(resource_type,resource_key,revision,updated_at) VALUES(?1,?2,?3,?4) ON CONFLICT(resource_type,resource_key) DO UPDATE SET revision=excluded.revision,updated_at=excluded.updated_at WHERE excluded.revision >= applied_resource_revisions.revision",
            )?
            .execute(rusqlite::params![
                resource.resource_type,
                resource.resource_key,
                revision,
                now
            ])?;
        let stored: i64 = transaction
            .prepare_cached(
                "SELECT revision FROM applied_resource_revisions WHERE resource_type=?1 AND resource_key=?2",
            )?
            .query_row(
                rusqlite::params![resource.resource_type, resource.resource_key],
                |row| row.get(0),
            )?;
        if stored != revision {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let updated = transaction
            .prepare_cached(
                "UPDATE command_journal SET state='succeeded',result=?3,error_code=NULL,privileged_result_proof=?4,updated_at=?5 WHERE idempotency_key=?1 AND command_id=?2 AND state='running'",
            )?
            .execute(rusqlite::params![
                idempotency_key.as_slice(),
                command_id.as_slice(),
                result,
                proof,
                now
            ])?;
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
        let mut stmt = self.connection.prepare_cached(
            "SELECT revision FROM applied_resource_revisions WHERE resource_type=?1 AND resource_key=?2",
        )?;
        let result = stmt.query_row(rusqlite::params![resource_type, resource_key], |row| {
            row.get::<_, i64>(0)
        });
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
        let mut statement = self.connection.prepare_cached(
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
            transaction
                .prepare_cached(
                    "INSERT INTO synthetic_effects(idempotency_key,payload_sha256,result,executed_at) VALUES (?1,?2,?3,?4)",
                )?
                .execute(rusqlite::params![
                    idempotency_key.as_slice(),
                    payload_sha256.as_slice(),
                    result,
                    now
                ])?;
            transaction
                .prepare_cached(
                    "UPDATE synthetic_effect_counter SET executions=executions+1 WHERE singleton=1",
                )?
                .execute([])?;
        }
        let updated = transaction
            .prepare_cached(
                "UPDATE command_journal SET state='succeeded',result=?4,error_code=NULL,updated_at=?5 WHERE idempotency_key=?1 AND command_id=?2 AND payload_sha256=?3 AND state='running'",
            )?
            .execute(rusqlite::params![
                idempotency_key.as_slice(),
                command_id.as_slice(),
                payload_sha256.as_slice(),
                result,
                now
            ])?;
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
        self.connection
            .prepare_cached("SELECT executions FROM synthetic_effect_counter WHERE singleton=1")?
            .query_row([], |row| row.get(0))
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

    /// Reads the durable connection-owner fencing epoch floor, or zero when
    /// no fence has been accepted yet.
    ///
    /// # Errors
    ///
    /// Returns a query or decoding error.
    pub fn owner_fence_epoch_floor(&self) -> Result<u64, rusqlite::Error> {
        let value: Option<Vec<u8>> = self
            .connection
            .prepare_cached("SELECT value FROM agent_metadata WHERE key=?1")?
            .query_row([OWNER_FENCE_EPOCH_FLOOR_KEY], |row| row.get(0))
            .optional()?;
        let Some(bytes) = value else {
            return Ok(0);
        };
        bytes
            .try_into()
            .map(u64::from_be_bytes)
            .map_err(|_| rusqlite::Error::InvalidQuery)
    }

    /// Raises the durable fencing epoch floor and returns the resulting
    /// persisted value. The stored value only ever increases, including
    /// across crashes and overlapping database connections, so a restarted
    /// Agent cannot be reset to accept an already-superseded owner epoch.
    ///
    /// # Errors
    ///
    /// Returns a write or decoding error.
    pub fn raise_owner_fence_epoch_floor(&self, epoch: u64) -> Result<u64, rusqlite::Error> {
        let value = epoch.to_be_bytes();
        let persisted: Vec<u8> = self
            .connection
            .prepare_cached(
                "INSERT INTO agent_metadata(key,value) VALUES(?1,?2)
                 ON CONFLICT(key) DO UPDATE SET value=CASE
                     WHEN excluded.value>agent_metadata.value THEN excluded.value
                     ELSE agent_metadata.value
                 END
                 RETURNING value",
            )?
            .query_row(
                rusqlite::params![OWNER_FENCE_EPOCH_FLOOR_KEY, value.as_slice()],
                |row| row.get(0),
            )?;
        persisted
            .try_into()
            .map(u64::from_be_bytes)
            .map_err(|_| rusqlite::Error::InvalidQuery)
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

fn decode_command_row(row: &rusqlite::Row<'_>) -> Result<CommandRecord, rusqlite::Error> {
    Ok(CommandRecord {
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
    })
}

fn query_command(
    connection: &Connection,
    idempotency_key: &[u8; 16],
) -> Result<Option<CommandRecord>, rusqlite::Error> {
    let mut statement = connection.prepare_cached(
        "SELECT idempotency_key,command_id,payload_sha256,payload_hash_version,state,result,error_code,privileged_result_proof,accepted_at,updated_at FROM command_journal WHERE idempotency_key=?1",
    )?;
    let mut rows = statement.query([idempotency_key.as_slice()])?;
    let Some(row) = rows.next()? else {
        return Ok(None);
    };
    decode_command_row(row).map(Some)
}

fn query_command_by_id(
    connection: &Connection,
    command_id: &[u8; 16],
) -> Result<Option<CommandRecord>, rusqlite::Error> {
    let mut statement = connection.prepare_cached(
        "SELECT idempotency_key,command_id,payload_sha256,payload_hash_version,state,result,error_code,privileged_result_proof,accepted_at,updated_at FROM command_journal WHERE command_id=?1",
    )?;
    let mut rows = statement.query([command_id.as_slice()])?;
    let Some(row) = rows.next()? else {
        return Ok(None);
    };
    decode_command_row(row).map(Some)
}

fn query_effect(
    connection: &Connection,
    idempotency_key: &[u8; 16],
) -> Result<Option<SyntheticEffect>, rusqlite::Error> {
    let mut statement = connection.prepare_cached(
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
