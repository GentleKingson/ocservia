use super::super::{
    AdapterError, AuthorizedEffectDecision, AuthorizedEffectIdentity, Connection,
    MAX_EFFECT_RECORDS, MAX_FRAME_BYTES, OptionalExtension, OwnerFenceIdentity,
    TransactionBehavior, Uuid, params,
};
use super::hmac::authenticate_authorized_record;
use super::owner_fence::{enforce_owner_fence, validate_owner_fence};
use super::{EffectStore, effect_now, fixed_blob, sqlite_io};

#[derive(Debug)]
pub(crate) struct AuthorizedEffectRecord {
    pub(crate) command_id: [u8; 16],
    pub(crate) idempotency_key: [u8; 16],
    pub(crate) node_id: [u8; 16],
    pub(crate) operation_id: [u8; 16],
    pub(crate) payload_sha256: [u8; 32],
    pub(crate) action: String,
    pub(crate) authorization_revision: u64,
    pub(crate) effect_kind: String,
    pub(crate) resource_key: String,
    pub(crate) effect_revision: u64,
    pub(crate) expires_at: i64,
    pub(crate) state: &'static str,
    pub(crate) delivery_mode: i64,
    pub(crate) response: Vec<u8>,
    pub(crate) record_hmac: [u8; 32],
}

#[derive(Clone, Copy)]
struct ValidatedAuthorizedEffect<'a> {
    command_id: &'a [u8; 16],
    idempotency_key: &'a [u8; 16],
    node_id: &'a [u8; 16],
    operation_id: &'a [u8; 16],
    payload_sha256: &'a [u8; 32],
    action: &'a str,
    authorization_revision: u64,
    effect_kind: &'a str,
    resource_key: &'a str,
    effect_revision: u64,
    expires_at: i64,
    delivery_mode: i64,
}

impl EffectStore {
    pub(crate) fn prepare_authorized(
        &mut self,
        identity: AuthorizedEffectIdentity<'_>,
        owner_fence: OwnerFenceIdentity<'_>,
    ) -> Result<AuthorizedEffectDecision, AdapterError> {
        let now = effect_now()?;
        let identity = validate_authorized_effect(identity, now)?;
        let owner_fence = validate_owner_fence(owner_fence)?;
        if identity.node_id != owner_fence.node_id {
            return Err(AdapterError::InvalidRequest);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        enforce_owner_fence(&transaction, &self.hmac_key, owner_fence, now)?;
        if let Some(mut existing) = query_authorized_effect(&transaction, identity.command_id)? {
            validate_authorized_record(&self.hmac_key, &existing)?;
            validate_same_authorized_effect(&existing, identity)?;
            if existing.state == "applied" {
                let response = existing.response;
                transaction.commit().map_err(sqlite_io)?;
                return Ok(AuthorizedEffectDecision::Replay(response));
            }
            if identity.delivery_mode == 3 && existing.delivery_mode == 1 {
                existing.delivery_mode = 3;
                existing.expires_at = identity.expires_at;
                existing.record_hmac = authenticate_authorized_record(&self.hmac_key, &existing);
                let changed = transaction
                    .execute(
                        "UPDATE authorized_effects SET expires_at=?2,delivery_mode=3,record_hmac=?3,updated_at=?4 WHERE command_id=?1 AND state='prepared' AND delivery_mode=1",
                        params![identity.command_id.as_slice(), identity.expires_at, existing.record_hmac.as_slice(), now],
                    )
                    .map_err(sqlite_io)?;
                if changed != 1 {
                    return Err(AdapterError::Unavailable);
                }
                transaction.commit().map_err(sqlite_io)?;
                return Ok(AuthorizedEffectDecision::Execute);
            }
            transaction.commit().map_err(sqlite_io)?;
            return Ok(AuthorizedEffectDecision::Pending);
        }
        let idempotency_owner: Option<Vec<u8>> = transaction
            .prepare_cached("SELECT command_id FROM authorized_effects WHERE idempotency_key=?1")
            .map_err(sqlite_io)?
            .query_row([identity.idempotency_key.as_slice()], |row| row.get(0))
            .optional()
            .map_err(sqlite_io)?;
        if idempotency_owner.is_some() {
            return Err(AdapterError::InvalidRequest);
        }
        // Preserve exact ID replay/conflict decisions above; only unrelated,
        // successful authorizations are safe to forget after expiry.
        prune_expired_authorized_effects(&transaction, &self.hmac_key, now)?;
        let count: i64 = transaction
            .query_row("SELECT count(*) FROM authorized_effects", [], |row| {
                row.get(0)
            })
            .map_err(sqlite_io)?;
        if count >= MAX_EFFECT_RECORDS {
            return Err(AdapterError::Unavailable);
        }
        let record = authorized_record(identity, "prepared", Vec::new(), [0; 32]);
        let record_hmac = authenticate_authorized_record(&self.hmac_key, &record);
        transaction
            .execute(
                "INSERT INTO authorized_effects(command_id,idempotency_key,node_id,operation_id,payload_sha256,action,authorization_revision,effect_kind,resource_key,effect_revision,expires_at,state,delivery_mode,response,record_hmac,updated_at)
                 VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,'prepared',?12,x'',?13,?14)",
                params![
                    identity.command_id.as_slice(),
                    identity.idempotency_key.as_slice(),
                    identity.node_id.as_slice(),
                    identity.operation_id.as_slice(),
                    identity.payload_sha256.as_slice(),
                    identity.action,
                    i64::try_from(identity.authorization_revision).map_err(|_| AdapterError::InvalidRequest)?,
                    identity.effect_kind,
                    identity.resource_key,
                    i64::try_from(identity.effect_revision).map_err(|_| AdapterError::InvalidRequest)?,
                    identity.expires_at,
                    identity.delivery_mode,
                    record_hmac.as_slice(),
                    now,
                ],
            )
            .map_err(sqlite_io)?;
        transaction.commit().map_err(sqlite_io)?;
        Ok(AuthorizedEffectDecision::Execute)
    }

    pub(crate) fn replay_authorized_after_fence(
        &mut self,
        identity: AuthorizedEffectIdentity<'_>,
        owner_fence: OwnerFenceIdentity<'_>,
    ) -> Result<Option<Vec<u8>>, AdapterError> {
        let now = effect_now()?;
        let identity = validate_authorized_effect(identity, 0)?;
        let owner_fence = validate_owner_fence(owner_fence)?;
        if identity.node_id != owner_fence.node_id {
            return Err(AdapterError::InvalidRequest);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        enforce_owner_fence(&transaction, &self.hmac_key, owner_fence, now)?;
        let record = query_authorized_effect(&transaction, identity.command_id)?;
        let response = if let Some(record) = record {
            validate_authorized_record(&self.hmac_key, &record)?;
            validate_same_authorized_effect(&record, identity)?;
            (record.state == "applied").then_some(record.response)
        } else {
            None
        };
        transaction.commit().map_err(sqlite_io)?;
        Ok(response)
    }

    pub(crate) fn complete_authorized(
        &mut self,
        identity: AuthorizedEffectIdentity<'_>,
        response: &[u8],
    ) -> Result<(), AdapterError> {
        if response.is_empty() || response.len() > MAX_FRAME_BYTES {
            return Err(AdapterError::OutputLimit);
        }
        let now = effect_now()?;
        // A root effect that completed just before expiry is still recorded.
        let identity = validate_authorized_effect(identity, 0)?;
        // Completion belongs to an effect already admitted by
        // `prepare_authorized`; a later takeover must not erase its receipt.
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let mut existing = query_authorized_effect(&transaction, identity.command_id)?
            .ok_or(AdapterError::Unavailable)?;
        validate_authorized_record(&self.hmac_key, &existing)?;
        validate_same_authorized_effect(&existing, identity)?;
        if existing.state == "applied" {
            if existing.response != response {
                return Err(AdapterError::InvalidRequest);
            }
            transaction.commit().map_err(sqlite_io)?;
            return Ok(());
        }
        existing.state = "applied";
        existing.response = response.to_vec();
        existing.expires_at = identity.expires_at;
        existing.delivery_mode = identity.delivery_mode;
        existing.record_hmac = authenticate_authorized_record(&self.hmac_key, &existing);
        let changed = transaction
            .execute(
                "UPDATE authorized_effects SET expires_at=?2,state='applied',delivery_mode=?3,response=?4,record_hmac=?5,updated_at=?6 WHERE command_id=?1 AND state='prepared'",
                params![identity.command_id.as_slice(), identity.expires_at, identity.delivery_mode, response, existing.record_hmac.as_slice(), now],
            )
            .map_err(sqlite_io)?;
        if changed != 1 {
            return Err(AdapterError::Unavailable);
        }
        transaction.commit().map_err(sqlite_io)
    }

    pub(crate) fn replay_authorized(
        &mut self,
        identity: AuthorizedEffectIdentity<'_>,
    ) -> Result<Option<Vec<u8>>, AdapterError> {
        let identity = validate_authorized_effect(identity, 0)?;
        let Some(record) = query_authorized_effect(&self.connection, identity.command_id)? else {
            return Ok(None);
        };
        validate_authorized_record(&self.hmac_key, &record)?;
        validate_same_authorized_effect(&record, identity)?;
        if record.state == "applied" {
            return Ok(Some(record.response));
        }
        Ok(None)
    }
}

fn validate_authorized_effect(
    identity: AuthorizedEffectIdentity<'_>,
    now: i64,
) -> Result<ValidatedAuthorizedEffect<'_>, AdapterError> {
    let command_id: &[u8; 16] = identity
        .command_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let idempotency_key: &[u8; 16] = identity
        .idempotency_key
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let node_id: &[u8; 16] = identity
        .node_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let operation_id: &[u8; 16] = identity
        .operation_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let payload_sha256: &[u8; 32] = identity
        .semantic_payload_sha256
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    if [command_id, idempotency_key, node_id, operation_id]
        .into_iter()
        .any(|value| Uuid::from_bytes(*value).get_version_num() != 7)
        || identity.authorization_revision == 0
        || identity.effect_revision == 0
        || identity.expires_at_unix_seconds <= now
        || !valid_effect_label(identity.action, 128)
        || !valid_effect_label(identity.effect_kind, 128)
        || !valid_effect_resource(identity.resource_key)
    {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(ValidatedAuthorizedEffect {
        command_id,
        idempotency_key,
        node_id,
        operation_id,
        payload_sha256,
        action: identity.action,
        authorization_revision: identity.authorization_revision,
        effect_kind: identity.effect_kind,
        resource_key: identity.resource_key,
        effect_revision: identity.effect_revision,
        expires_at: identity.expires_at_unix_seconds,
        delivery_mode: if identity.retry_if_absent { 3 } else { 1 },
    })
}

fn valid_effect_label(value: &str, maximum: usize) -> bool {
    !value.is_empty()
        && value.len() <= maximum
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
}

fn valid_effect_resource(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 512
        && value
            .bytes()
            .all(|byte| byte.is_ascii_graphic() && byte != b'\\')
}

fn authorized_record(
    identity: ValidatedAuthorizedEffect<'_>,
    state: &'static str,
    response: Vec<u8>,
    record_hmac: [u8; 32],
) -> AuthorizedEffectRecord {
    AuthorizedEffectRecord {
        command_id: *identity.command_id,
        idempotency_key: *identity.idempotency_key,
        node_id: *identity.node_id,
        operation_id: *identity.operation_id,
        payload_sha256: *identity.payload_sha256,
        action: identity.action.to_owned(),
        authorization_revision: identity.authorization_revision,
        effect_kind: identity.effect_kind.to_owned(),
        resource_key: identity.resource_key.to_owned(),
        effect_revision: identity.effect_revision,
        expires_at: identity.expires_at,
        state,
        delivery_mode: identity.delivery_mode,
        response,
        record_hmac,
    }
}

fn decode_authorized_row(
    row: &rusqlite::Row<'_>,
) -> Result<AuthorizedEffectRecord, rusqlite::Error> {
    let authorization_revision: i64 = row.get(6)?;
    let effect_revision: i64 = row.get(9)?;
    Ok(AuthorizedEffectRecord {
        command_id: fixed_blob(row.get(0)?)?,
        idempotency_key: fixed_blob(row.get(1)?)?,
        node_id: fixed_blob(row.get(2)?)?,
        operation_id: fixed_blob(row.get(3)?)?,
        payload_sha256: fixed_blob(row.get(4)?)?,
        action: row.get(5)?,
        authorization_revision: u64::try_from(authorization_revision)
            .map_err(|_| rusqlite::Error::InvalidQuery)?,
        effect_kind: row.get(7)?,
        resource_key: row.get(8)?,
        effect_revision: u64::try_from(effect_revision)
            .map_err(|_| rusqlite::Error::InvalidQuery)?,
        expires_at: row.get(10)?,
        state: match row.get::<_, String>(11)?.as_str() {
            "prepared" => "prepared",
            "applied" => "applied",
            _ => return Err(rusqlite::Error::InvalidQuery),
        },
        delivery_mode: row.get(12)?,
        response: row.get(13)?,
        record_hmac: fixed_blob(row.get(14)?)?,
    })
}

pub(crate) fn query_authorized_effect(
    connection: &Connection,
    command_id: &[u8; 16],
) -> Result<Option<AuthorizedEffectRecord>, AdapterError> {
    let mut statement = connection
        .prepare_cached(
            "SELECT command_id,idempotency_key,node_id,operation_id,payload_sha256,action,authorization_revision,effect_kind,resource_key,effect_revision,expires_at,state,delivery_mode,response,record_hmac FROM authorized_effects WHERE command_id=?1",
        )
        .map_err(sqlite_io)?;
    statement
        .query_row([command_id.as_slice()], decode_authorized_row)
        .optional()
        .map_err(sqlite_io)
}

fn prune_expired_authorized_effects(
    connection: &Connection,
    key: &[u8; 32],
    now: i64,
) -> Result<(), AdapterError> {
    let records = {
        let mut statement = connection
            .prepare_cached(
                "SELECT command_id,idempotency_key,node_id,operation_id,payload_sha256,action,authorization_revision,effect_kind,resource_key,effect_revision,expires_at,state,delivery_mode,response,record_hmac FROM authorized_effects WHERE state='applied' AND expires_at<=?1",
            )
            .map_err(sqlite_io)?;
        let rows = statement
            .query_map([now], decode_authorized_row)
            .map_err(sqlite_io)?;
        rows.collect::<Result<Vec<_>, _>>().map_err(sqlite_io)?
    };
    if records.is_empty() {
        return Ok(());
    }
    let mut delete_stmt = connection
        .prepare_cached(
            "DELETE FROM authorized_effects WHERE command_id=?1 AND state='applied' AND expires_at<=?2",
        )
        .map_err(sqlite_io)?;
    for record in records {
        validate_authorized_record(key, &record)?;
        if record.state != "applied" || record.expires_at > now {
            return Err(AdapterError::InvalidResource);
        }
        let changed = delete_stmt
            .execute(params![record.command_id.as_slice(), now])
            .map_err(sqlite_io)?;
        if changed != 1 {
            return Err(AdapterError::Unavailable);
        }
    }
    Ok(())
}

fn validate_same_authorized_effect(
    record: &AuthorizedEffectRecord,
    identity: ValidatedAuthorizedEffect<'_>,
) -> Result<(), AdapterError> {
    if record.command_id != *identity.command_id
        || record.idempotency_key != *identity.idempotency_key
        || record.node_id != *identity.node_id
        || record.operation_id != *identity.operation_id
        || record.payload_sha256 != *identity.payload_sha256
        || record.action != identity.action
        || record.authorization_revision != identity.authorization_revision
        || record.effect_kind != identity.effect_kind
        || record.resource_key != identity.resource_key
        || record.effect_revision != identity.effect_revision
    {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(())
}

fn validate_authorized_record(
    key: &[u8; 32],
    record: &AuthorizedEffectRecord,
) -> Result<(), AdapterError> {
    if record.response.len() > MAX_FRAME_BYTES
        || !matches!(record.state, "prepared" | "applied")
        || !matches!(record.delivery_mode, 1 | 3)
        || authenticate_authorized_record(key, record) != record.record_hmac
    {
        return Err(AdapterError::InvalidResource);
    }
    Ok(())
}
