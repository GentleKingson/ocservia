use rusqlite::TransactionBehavior;

use crate::Journal;

/// Maximum time that telemetry remains buffered while an Agent is offline.
pub const OFFLINE_RECOVERY_RETENTION_SECONDS: u64 = 300;

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
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        prune_expired_telemetry(&transaction, now)?;
        {
            let mut insert_stmt = transaction.prepare_cached(
                "INSERT INTO telemetry_buffer (batch_id,priority,observed_at,expires_at,payload) VALUES (?1,?2,?3,?4,?5) ON CONFLICT(batch_id) DO NOTHING",
            )?;
            insert_stmt.execute(rusqlite::params![
                batch_id.as_slice(),
                priority,
                observed_at,
                expires_at,
                payload
            ])?;
        }
        let mut bytes: u64 = {
            let mut sum_stmt = transaction
                .prepare_cached("SELECT coalesce(sum(length(payload)),0) FROM telemetry_buffer")?;
            sum_stmt.query_row([], |row| row.get(0))?
        };
        let mut evicted_drops = [0_u64; 4];
        let mut self_evicted = false;
        if bytes > max_bytes {
            let mut evict_stmt = transaction.prepare_cached(
                "DELETE FROM telemetry_buffer WHERE batch_id=(SELECT batch_id FROM telemetry_buffer ORDER BY priority DESC,observed_at ASC,batch_id LIMIT 1) RETURNING batch_id,priority,length(payload)",
            )?;
            while bytes > max_bytes {
                let (evicted_id, evicted_priority, evicted_len): (Vec<u8>, u8, u64) =
                    evict_stmt.query_row([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))?;
                bytes = bytes.saturating_sub(evicted_len);
                if (1..=4).contains(&evicted_priority) {
                    evicted_drops[usize::from(evicted_priority - 1)] += 1;
                }
                if evicted_id.as_slice() == batch_id.as_slice() {
                    self_evicted = true;
                }
            }
        }
        for (idx, count) in evicted_drops.into_iter().enumerate() {
            if count > 0 {
                let evicted_priority =
                    u8::try_from(idx + 1).map_err(|_| rusqlite::Error::InvalidQuery)?;
                increment_drop(&transaction, evicted_priority, count)?;
            }
        }
        transaction.commit()?;
        Ok(!self_evicted)
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
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        prune_expired_telemetry(&transaction, now)?;
        let pending = {
            let mut statement = transaction.prepare_cached(
                "SELECT batch_id,payload FROM telemetry_buffer ORDER BY priority,observed_at,batch_id LIMIT ?1",
            )?;
            statement
                .query_map([limit], |row| {
                    let id = row.get::<_, Vec<u8>>(0)?;
                    let id: [u8; 16] = id.try_into().map_err(|_| rusqlite::Error::InvalidQuery)?;
                    Ok((id, row.get(1)?))
                })?
                .collect::<Result<Vec<_>, _>>()?
        };
        transaction.commit()?;
        Ok(pending)
    }

    /// Removes multiple delivered telemetry batches idempotently in a single
    /// transaction so buffered backlog drains incur one `synchronous=FULL`
    /// commit instead of one commit per batch.
    ///
    /// # Errors
    ///
    /// Returns a `SQLite` failure.
    pub fn acknowledge_telemetry_batch(
        &mut self,
        batch_ids: &[[u8; 16]],
    ) -> Result<(), rusqlite::Error> {
        if batch_ids.is_empty() {
            return Ok(());
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        {
            let mut stmt =
                transaction.prepare_cached("DELETE FROM telemetry_buffer WHERE batch_id=?1")?;
            for batch_id in batch_ids {
                stmt.execute([batch_id.as_slice()])?;
            }
        }
        transaction.commit()?;
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
            .prepare_cached("SELECT priority,dropped FROM telemetry_drop_counters")?;
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
    let mut stmt = transaction.prepare_cached(
        "INSERT INTO telemetry_drop_counters(priority,dropped) VALUES (?1,?2) ON CONFLICT(priority) DO UPDATE SET dropped=dropped+excluded.dropped",
    )?;
    stmt.execute(rusqlite::params![priority, count])?;
    Ok(())
}

fn prune_expired_telemetry(
    transaction: &rusqlite::Transaction<'_>,
    now: i64,
) -> Result<(), rusqlite::Error> {
    let recovery_seconds = i64::try_from(OFFLINE_RECOVERY_RETENTION_SECONDS)
        .map_err(|_| rusqlite::Error::InvalidQuery)?;
    let observed_cutoff = now.saturating_sub(recovery_seconds);
    let mut expired_drops = [0_u64; 4];
    {
        let mut stmt = transaction.prepare_cached(
            "DELETE FROM telemetry_buffer WHERE expires_at <= ?1 OR observed_at <= ?2 RETURNING priority",
        )?;
        let rows = stmt.query_map(rusqlite::params![now, observed_cutoff], |row| {
            row.get::<_, u8>(0)
        })?;
        for row in rows {
            let expired_priority = row?;
            if (1..=4).contains(&expired_priority) {
                expired_drops[usize::from(expired_priority - 1)] += 1;
            }
        }
    }
    for (idx, count) in expired_drops.into_iter().enumerate() {
        if count > 0 {
            let expired_priority =
                u8::try_from(idx + 1).map_err(|_| rusqlite::Error::InvalidQuery)?;
            increment_drop(transaction, expired_priority, count)?;
        }
    }
    Ok(())
}
