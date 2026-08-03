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
                ON command_journal(updated_at);
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
             ) STRICT;",
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
        let mut expired = transaction.prepare(
            "SELECT priority, count(*) FROM telemetry_buffer WHERE expires_at <= ?1 GROUP BY priority",
        )?;
        let expired_counts = expired
            .query_map([now], |row| {
                Ok((row.get::<_, u8>(0)?, row.get::<_, u64>(1)?))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        drop(expired);
        transaction.execute("DELETE FROM telemetry_buffer WHERE expires_at <= ?1", [now])?;
        for (expired_priority, count) in expired_counts {
            increment_drop(&transaction, expired_priority, count)?;
        }
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

    /// Returns buffered payloads in delivery priority and observation order.
    ///
    /// # Errors
    ///
    /// Returns a query error.
    pub fn telemetry_pending(
        &self,
        limit: usize,
    ) -> Result<Vec<PendingTelemetry>, rusqlite::Error> {
        if limit == 0 || limit > 256 {
            return Err(rusqlite::Error::InvalidQuery);
        }
        let mut statement = self.connection.prepare(
            "SELECT batch_id,payload FROM telemetry_buffer ORDER BY priority,observed_at,batch_id LIMIT ?1",
        )?;
        statement
            .query_map([limit], |row| {
                let id = row.get::<_, Vec<u8>>(0)?;
                let id: [u8; 16] = id.try_into().map_err(|_| rusqlite::Error::InvalidQuery)?;
                Ok((id, row.get(1)?))
            })?
            .collect()
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
}

fn increment_drop(
    transaction: &rusqlite::Transaction<'_>,
    priority: u8,
    count: u64,
) -> Result<(), rusqlite::Error> {
    transaction.execute("INSERT INTO telemetry_drop_counters(priority,dropped) VALUES (?1,?2) ON CONFLICT(priority) DO UPDATE SET dropped=dropped+excluded.dropped",rusqlite::params![priority,count])?;
    Ok(())
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
        let pending = journal.telemetry_pending(10).expect("pending");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].0, security);
        assert_eq!(
            journal.telemetry_drop_counters().expect("drops"),
            [0, 0, 0, 1]
        );
        journal.acknowledge_telemetry(&security).expect("ack");
        assert!(journal.telemetry_pending(10).expect("empty").is_empty());
        drop(journal);
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{}", path.display(), suffix));
        }
    }
}
