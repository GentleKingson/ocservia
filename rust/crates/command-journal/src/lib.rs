//! Durable bounded Agent state and command-journal foundation.

#![forbid(unsafe_code)]

use std::path::Path;
use std::time::Duration;

use rusqlite::{Connection, OpenFlags};

const BUSY_TIMEOUT: Duration = Duration::from_secs(5);

/// SQLite-backed Agent local state.
#[derive(Debug)]
pub struct Journal {
    connection: Connection,
}

impl Journal {
    /// Opens an owner-controlled database and enforces the required `SQLite` pragmas.
    ///
    /// # Errors
    ///
    /// Returns an error when the database cannot be opened, configured, or migrated.
    pub fn open(path: &Path) -> Result<Self, rusqlite::Error> {
        let connection = Connection::open_with_flags(
            path,
            OpenFlags::SQLITE_OPEN_READ_WRITE
                | OpenFlags::SQLITE_OPEN_CREATE
                | OpenFlags::SQLITE_OPEN_NO_MUTEX
                | OpenFlags::SQLITE_OPEN_NOFOLLOW,
        )?;
        connection.busy_timeout(BUSY_TIMEOUT)?;
        connection.pragma_update(None, "journal_mode", "WAL")?;
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
                state TEXT NOT NULL CHECK (state IN ('accepted','running','succeeded','failed','unknown')),
                result BLOB,
                accepted_at INTEGER NOT NULL,
                updated_at INTEGER NOT NULL,
                CHECK (length(idempotency_key) = 16),
                CHECK (length(command_id) = 16),
                CHECK (length(payload_sha256) = 32),
                CHECK (result IS NULL OR length(result) <= 1048576)
             ) STRICT;
             CREATE INDEX IF NOT EXISTS command_journal_updated_at
                ON command_journal(updated_at);",
        )?;
        Ok(Self { connection })
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
}

#[cfg(test)]
mod tests {
    use super::*;

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
        drop(journal);
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
        }
    }
}
