use super::super::{
    AdapterError, Connection, DesiredEffectObservation, DesiredEffectState, EffectIdentity,
    MAX_EFFECT_RECORDS, OptionalExtension, TransactionBehavior, Uuid, params, validate_name,
};
use super::hmac::{authenticate_file, authenticate_record, verify_mac};
use super::{EffectStore, effect_now, fixed_blob, sqlite_io};

pub(crate) fn validate_effect_identity(
    mutation_kind: &str,
    resource_key: &str,
    desired_revision: u64,
) -> Result<(), AdapterError> {
    if !matches!(
        mutation_kind,
        "user_create"
            | "user_disable"
            | "user_enable"
            | "user_password_rotate"
            | "group_apply"
            | "config_apply"
    ) || desired_revision == 0
    {
        return Err(AdapterError::InvalidRequest);
    }
    validate_name(resource_key)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PrepareEffect {
    Proceed,
    AlreadyApplied,
}

#[derive(Debug)]
struct EffectRecord {
    mutation_kind: String,
    resource_key: String,
    revision: u64,
    command_id: [u8; 16],
    idempotency_key: [u8; 16],
    payload_sha256: [u8; 32],
    expires_at: i64,
    state: &'static str,
    before_hmac: [u8; 32],
    after_hmac: [u8; 32],
    record_hmac: [u8; 32],
}

#[derive(Clone, Copy)]
pub(crate) struct ValidatedEffectIdentity<'a> {
    pub(crate) command_id: &'a [u8; 16],
    pub(crate) idempotency_key: &'a [u8; 16],
    pub(crate) payload_sha256: &'a [u8; 32],
    pub(crate) expires_at: i64,
}

impl EffectStore {
    pub(crate) fn prepare(
        &mut self,
        mutation_kind: &str,
        resource_key: &str,
        revision: u64,
        identity: EffectIdentity<'_>,
        before: &[u8],
        after: &[u8],
    ) -> Result<PrepareEffect, AdapterError> {
        let now = effect_now()?;
        let identity =
            validate_effect_context(mutation_kind, resource_key, revision, identity, now)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let current_hmac = authenticate_file(&self.hmac_key, before);
        let prepared = query_prepared_effects(&transaction, mutation_kind == "config_apply")?;
        for record in prepared {
            validate_effect_record(&self.hmac_key, &record)?;
            if current_hmac == record.after_hmac {
                if record.mutation_kind != "config_apply" {
                    update_effect_state(&transaction, &self.hmac_key, record, "applied", now)?;
                }
                continue;
            }
            if record.expires_at <= now && current_hmac == record.before_hmac {
                update_effect_state(&transaction, &self.hmac_key, record, "absent", now)?;
                continue;
            }
            let same_pending = record.mutation_kind == mutation_kind
                && record.resource_key == resource_key
                && record.revision == revision
                && validate_same_effect(&record, identity).is_ok();
            if current_hmac != record.before_hmac || !same_pending {
                return Err(AdapterError::Unavailable);
            }
        }
        if let Some(existing) = query_effect(&transaction, mutation_kind, resource_key)? {
            validate_effect_record(&self.hmac_key, &existing)?;
            if existing.revision > revision {
                return Err(AdapterError::InvalidRequest);
            }
            if existing.revision == revision {
                validate_same_effect(&existing, identity)?;
                if existing.state == "applied" {
                    transaction.commit().map_err(sqlite_io)?;
                    return Ok(PrepareEffect::AlreadyApplied);
                }
                if existing.state == "prepared" {
                    if current_hmac == existing.after_hmac {
                        if mutation_kind == "config_apply" {
                            transaction.commit().map_err(sqlite_io)?;
                            return Ok(PrepareEffect::Proceed);
                        }
                        update_effect_state(
                            &transaction,
                            &self.hmac_key,
                            existing,
                            "applied",
                            now,
                        )?;
                        transaction.commit().map_err(sqlite_io)?;
                        return Ok(PrepareEffect::AlreadyApplied);
                    }
                    if current_hmac != existing.before_hmac {
                        return Err(AdapterError::Unavailable);
                    }
                }
            }
        } else {
            let count: i64 = transaction
                .query_row("SELECT count(*) FROM desired_effects", [], |row| row.get(0))
                .map_err(sqlite_io)?;
            if count >= MAX_EFFECT_RECORDS {
                return Err(AdapterError::Unavailable);
            }
        }
        let before_hmac = current_hmac;
        let after_hmac = authenticate_file(&self.hmac_key, after);
        let record_hmac = authenticate_record(
            &self.hmac_key,
            mutation_kind,
            resource_key,
            revision,
            identity,
            "prepared",
            &before_hmac,
            &after_hmac,
        );
        transaction
            .execute(
                "INSERT INTO desired_effects(mutation_kind,resource_key,revision,command_id,idempotency_key,payload_sha256,expires_at,state,before_hmac,after_hmac,record_hmac,updated_at)
                 VALUES(?1,?2,?3,?4,?5,?6,?7,'prepared',?8,?9,?10,?11)
                 ON CONFLICT(mutation_kind,resource_key) DO UPDATE SET revision=excluded.revision,command_id=excluded.command_id,idempotency_key=excluded.idempotency_key,payload_sha256=excluded.payload_sha256,expires_at=excluded.expires_at,state=excluded.state,before_hmac=excluded.before_hmac,after_hmac=excluded.after_hmac,record_hmac=excluded.record_hmac,updated_at=excluded.updated_at",
                params![mutation_kind, resource_key, i64::try_from(revision).map_err(|_| AdapterError::InvalidRequest)?, identity.command_id.as_slice(), identity.idempotency_key.as_slice(), identity.payload_sha256.as_slice(), identity.expires_at, before_hmac.as_slice(), after_hmac.as_slice(), record_hmac.as_slice(), now],
            )
            .map_err(sqlite_io)?;
        transaction.commit().map_err(sqlite_io)?;
        Ok(PrepareEffect::Proceed)
    }

    pub(crate) fn mark_applied(
        &mut self,
        mutation_kind: &str,
        resource_key: &str,
        revision: u64,
        identity: EffectIdentity<'_>,
    ) -> Result<(), AdapterError> {
        let now = effect_now()?;
        // Expiry is checked before execution. Do not turn a successfully
        // committed effect into a terminal rejection if it expires mid-call.
        let identity = validate_effect_context(mutation_kind, resource_key, revision, identity, 0)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let record = query_effect(&transaction, mutation_kind, resource_key)?
            .ok_or(AdapterError::Unavailable)?;
        validate_effect_record(&self.hmac_key, &record)?;
        validate_same_effect(&record, identity)?;
        update_effect_state(&transaction, &self.hmac_key, record, "applied", now)?;
        transaction.commit().map_err(sqlite_io)
    }

    pub(crate) fn mark_absent(
        &mut self,
        mutation_kind: &str,
        resource_key: &str,
        revision: u64,
        identity: EffectIdentity<'_>,
    ) -> Result<(), AdapterError> {
        let now = effect_now()?;
        let identity = validate_effect_context(mutation_kind, resource_key, revision, identity, 0)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let record = query_effect(&transaction, mutation_kind, resource_key)?
            .ok_or(AdapterError::Unavailable)?;
        validate_effect_record(&self.hmac_key, &record)?;
        validate_same_effect(&record, identity)?;
        update_effect_state(&transaction, &self.hmac_key, record, "absent", now)?;
        transaction.commit().map_err(sqlite_io)
    }

    pub(crate) fn observe(
        &mut self,
        mutation_kind: &str,
        resource_key: &str,
        revision: u64,
        identity: EffectIdentity<'_>,
        current: &[u8],
    ) -> Result<DesiredEffectObservation, AdapterError> {
        let now = effect_now()?;
        let identity = validate_effect_context(mutation_kind, resource_key, revision, identity, 0)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let Some(record) = query_effect(&transaction, mutation_kind, resource_key)? else {
            transaction.commit().map_err(sqlite_io)?;
            return Ok(effect_observation(DesiredEffectState::Unknown, 0));
        };
        validate_effect_record(&self.hmac_key, &record)?;
        if record.revision > revision {
            let observed = record.revision;
            transaction.commit().map_err(sqlite_io)?;
            return Ok(effect_observation(
                DesiredEffectState::SupersededByNewerRevision,
                observed,
            ));
        }
        if record.revision < revision {
            let observed = record.revision;
            transaction.commit().map_err(sqlite_io)?;
            return Ok(effect_observation(DesiredEffectState::Unknown, observed));
        }
        if validate_same_effect(&record, identity).is_err() {
            let observed = record.revision;
            transaction.commit().map_err(sqlite_io)?;
            return Ok(effect_observation(DesiredEffectState::Unknown, observed));
        }
        if record.state == "applied" {
            transaction.commit().map_err(sqlite_io)?;
            return Ok(effect_observation(
                DesiredEffectState::AppliedExact,
                revision,
            ));
        }
        if record.state == "absent" {
            transaction.commit().map_err(sqlite_io)?;
            return Ok(effect_observation(DesiredEffectState::Absent, revision));
        }
        let current_hmac = authenticate_file(&self.hmac_key, current);
        let state = if current_hmac == record.after_hmac && mutation_kind == "config_apply" {
            DesiredEffectState::Absent
        } else if current_hmac == record.after_hmac {
            update_effect_state(&transaction, &self.hmac_key, record, "applied", now)?;
            DesiredEffectState::AppliedExact
        } else if current_hmac == record.before_hmac {
            update_effect_state(&transaction, &self.hmac_key, record, "absent", now)?;
            DesiredEffectState::Absent
        } else {
            DesiredEffectState::Unknown
        };
        transaction.commit().map_err(sqlite_io)?;
        Ok(effect_observation(state, revision))
    }
}

fn effect_observation(state: DesiredEffectState, revision: u64) -> DesiredEffectObservation {
    DesiredEffectObservation {
        state: state.into(),
        observed_revision: revision,
    }
}

pub(crate) fn validate_effect_context<'a>(
    mutation_kind: &str,
    resource_key: &str,
    revision: u64,
    identity: EffectIdentity<'a>,
    now: i64,
) -> Result<ValidatedEffectIdentity<'a>, AdapterError> {
    validate_effect_identity(mutation_kind, resource_key, revision)?;
    let command_id: &'a [u8; 16] = identity
        .command_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let idempotency_key: &'a [u8; 16] = identity
        .idempotency_key
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let payload_sha256: &'a [u8; 32] = identity
        .semantic_payload_sha256
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    if Uuid::from_bytes(*command_id).get_version_num() != 7
        || Uuid::from_bytes(*idempotency_key).get_version_num() != 7
        || identity.expires_at_unix_seconds <= now
    {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(ValidatedEffectIdentity {
        command_id,
        idempotency_key,
        payload_sha256,
        expires_at: identity.expires_at_unix_seconds,
    })
}

fn query_effect(
    connection: &Connection,
    mutation_kind: &str,
    resource_key: &str,
) -> Result<Option<EffectRecord>, AdapterError> {
    let mut statement = connection
        .prepare_cached(
            "SELECT mutation_kind,resource_key,revision,command_id,idempotency_key,payload_sha256,expires_at,state,before_hmac,after_hmac,record_hmac FROM desired_effects WHERE mutation_kind=?1 AND resource_key=?2",
        )
        .map_err(sqlite_io)?;
    statement
        .query_row(params![mutation_kind, resource_key], |row| {
            let revision: i64 = row.get(2)?;
            Ok(EffectRecord {
                mutation_kind: row.get(0)?,
                resource_key: row.get(1)?,
                revision: u64::try_from(revision).map_err(|_| rusqlite::Error::InvalidQuery)?,
                command_id: fixed_blob(row.get(3)?)?,
                idempotency_key: fixed_blob(row.get(4)?)?,
                payload_sha256: fixed_blob(row.get(5)?)?,
                expires_at: row.get(6)?,
                state: match row.get::<_, String>(7)?.as_str() {
                    "prepared" => "prepared",
                    "applied" => "applied",
                    "absent" => "absent",
                    _ => return Err(rusqlite::Error::InvalidQuery),
                },
                before_hmac: fixed_blob(row.get(8)?)?,
                after_hmac: fixed_blob(row.get(9)?)?,
                record_hmac: fixed_blob(row.get(10)?)?,
            })
        })
        .optional()
        .map_err(sqlite_io)
}

fn query_prepared_effects(
    connection: &Connection,
    config_effects: bool,
) -> Result<Vec<EffectRecord>, AdapterError> {
    let mut statement = connection
        .prepare_cached(
            "SELECT mutation_kind,resource_key,revision,command_id,idempotency_key,payload_sha256,expires_at,state,before_hmac,after_hmac,record_hmac FROM desired_effects WHERE state='prepared' AND (mutation_kind='config_apply')=?1 ORDER BY mutation_kind,resource_key",
        )
        .map_err(sqlite_io)?;
    let rows = statement
        .query_map([config_effects], |row| {
            let revision: i64 = row.get(2)?;
            Ok(EffectRecord {
                mutation_kind: row.get(0)?,
                resource_key: row.get(1)?,
                revision: u64::try_from(revision).map_err(|_| rusqlite::Error::InvalidQuery)?,
                command_id: fixed_blob(row.get(3)?)?,
                idempotency_key: fixed_blob(row.get(4)?)?,
                payload_sha256: fixed_blob(row.get(5)?)?,
                expires_at: row.get(6)?,
                state: match row.get::<_, String>(7)?.as_str() {
                    "prepared" => "prepared",
                    _ => return Err(rusqlite::Error::InvalidQuery),
                },
                before_hmac: fixed_blob(row.get(8)?)?,
                after_hmac: fixed_blob(row.get(9)?)?,
                record_hmac: fixed_blob(row.get(10)?)?,
            })
        })
        .map_err(sqlite_io)?;
    rows.collect::<Result<Vec<_>, _>>().map_err(sqlite_io)
}

fn validate_same_effect(
    record: &EffectRecord,
    identity: ValidatedEffectIdentity<'_>,
) -> Result<(), AdapterError> {
    if record.command_id != *identity.command_id
        || record.idempotency_key != *identity.idempotency_key
        || record.payload_sha256 != *identity.payload_sha256
    {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(())
}

fn validate_effect_record(key: &[u8; 32], record: &EffectRecord) -> Result<(), AdapterError> {
    let identity = ValidatedEffectIdentity {
        command_id: &record.command_id,
        idempotency_key: &record.idempotency_key,
        payload_sha256: &record.payload_sha256,
        expires_at: record.expires_at,
    };
    let expected = authenticate_record(
        key,
        &record.mutation_kind,
        &record.resource_key,
        record.revision,
        identity,
        record.state,
        &record.before_hmac,
        &record.after_hmac,
    );
    if !verify_mac(&expected, &record.record_hmac) {
        return Err(AdapterError::InvalidResource);
    }
    Ok(())
}

fn update_effect_state(
    connection: &Connection,
    key: &[u8; 32],
    mut record: EffectRecord,
    state: &'static str,
    now: i64,
) -> Result<(), AdapterError> {
    record.state = state;
    let identity = ValidatedEffectIdentity {
        command_id: &record.command_id,
        idempotency_key: &record.idempotency_key,
        payload_sha256: &record.payload_sha256,
        expires_at: record.expires_at,
    };
    let record_hmac = authenticate_record(
        key,
        &record.mutation_kind,
        &record.resource_key,
        record.revision,
        identity,
        state,
        &record.before_hmac,
        &record.after_hmac,
    );
    let mut statement = connection
        .prepare_cached(
            "UPDATE desired_effects SET state=?4,record_hmac=?5,updated_at=?6 WHERE mutation_kind=?1 AND resource_key=?2 AND revision=?3",
        )
        .map_err(sqlite_io)?;
    let changed = statement
        .execute(params![
            record.mutation_kind,
            record.resource_key,
            i64::try_from(record.revision).map_err(|_| AdapterError::InvalidResource)?,
            state,
            record_hmac.as_slice(),
            now
        ])
        .map_err(sqlite_io)?;
    if changed != 1 {
        return Err(AdapterError::Unavailable);
    }
    Ok(())
}
