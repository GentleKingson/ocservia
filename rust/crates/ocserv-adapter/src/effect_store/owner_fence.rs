use super::super::{
    AdapterError, OptionalExtension, OwnerFenceIdentity, TransactionBehavior, Uuid, params,
};
use super::hmac::authenticate_owner_fence_record;
use super::{EffectStore, effect_now, fixed_blob, sqlite_io};

#[derive(Debug)]
pub(crate) struct OwnerFenceRecord {
    pub(crate) node_id: [u8; 16],
    pub(crate) endpoint_id: [u8; 32],
    pub(crate) owner_epoch: u64,
    pub(crate) record_hmac: [u8; 32],
}

#[derive(Clone, Copy)]
pub(crate) struct ValidatedOwnerFence<'a> {
    pub(crate) node_id: &'a [u8; 16],
    pub(crate) endpoint_id: &'a [u8; 32],
    pub(crate) owner_epoch: u64,
}

impl EffectStore {
    pub(crate) fn accept_owner_fence(
        &mut self,
        identity: OwnerFenceIdentity<'_>,
    ) -> Result<(), AdapterError> {
        let now = effect_now()?;
        let identity = validate_owner_fence(identity)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        enforce_owner_fence(&transaction, &self.hmac_key, identity, now)?;
        transaction.commit().map_err(sqlite_io)
    }
}

pub(crate) fn validate_owner_fence(
    identity: OwnerFenceIdentity<'_>,
) -> Result<ValidatedOwnerFence<'_>, AdapterError> {
    let node_id: &[u8; 16] = identity
        .node_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let endpoint_id: &[u8; 32] = identity
        .endpoint_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    if Uuid::from_bytes(*node_id).get_version_num() != 7
        || identity.owner_epoch == 0
        || i64::try_from(identity.owner_epoch).is_err()
    {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(ValidatedOwnerFence {
        node_id,
        endpoint_id,
        owner_epoch: identity.owner_epoch,
    })
}

pub(crate) fn enforce_owner_fence(
    transaction: &rusqlite::Transaction<'_>,
    hmac_key: &[u8; 32],
    identity: ValidatedOwnerFence<'_>,
    now: i64,
) -> Result<(), AdapterError> {
    let existing = transaction
        .prepare_cached(
            "SELECT node_id,endpoint_id,owner_epoch,record_hmac
             FROM owner_epoch_floors WHERE node_id=?1 AND endpoint_id=?2",
        )
        .map_err(sqlite_io)?
        .query_row(
            params![identity.node_id.as_slice(), identity.endpoint_id.as_slice()],
            |row| {
                let owner_epoch: i64 = row.get(2)?;
                Ok(OwnerFenceRecord {
                    node_id: fixed_blob(row.get(0)?)?,
                    endpoint_id: fixed_blob(row.get(1)?)?,
                    owner_epoch: u64::try_from(owner_epoch)
                        .map_err(|_| rusqlite::Error::InvalidQuery)?,
                    record_hmac: fixed_blob(row.get(3)?)?,
                })
            },
        )
        .optional()
        .map_err(sqlite_io)?;
    if let Some(mut record) = existing {
        if authenticate_owner_fence_record(hmac_key, &record) != record.record_hmac {
            return Err(AdapterError::InvalidResource);
        }
        if identity.owner_epoch < record.owner_epoch {
            return Err(AdapterError::StaleOwnerEpoch);
        }
        if identity.owner_epoch == record.owner_epoch {
            return Ok(());
        }
        record.owner_epoch = identity.owner_epoch;
        record.record_hmac = authenticate_owner_fence_record(hmac_key, &record);
        let changed = transaction
            .execute(
                "UPDATE owner_epoch_floors SET owner_epoch=?3,record_hmac=?4,updated_at=?5
                 WHERE node_id=?1 AND endpoint_id=?2",
                params![
                    identity.node_id.as_slice(),
                    identity.endpoint_id.as_slice(),
                    i64::try_from(identity.owner_epoch)
                        .map_err(|_| AdapterError::InvalidRequest)?,
                    record.record_hmac.as_slice(),
                    now,
                ],
            )
            .map_err(sqlite_io)?;
        if changed != 1 {
            return Err(AdapterError::Unavailable);
        }
        return Ok(());
    }

    let mut record = OwnerFenceRecord {
        node_id: *identity.node_id,
        endpoint_id: *identity.endpoint_id,
        owner_epoch: identity.owner_epoch,
        record_hmac: [0; 32],
    };
    record.record_hmac = authenticate_owner_fence_record(hmac_key, &record);
    transaction
        .execute(
            "INSERT INTO owner_epoch_floors(node_id,endpoint_id,owner_epoch,record_hmac,updated_at)
             VALUES(?1,?2,?3,?4,?5)",
            params![
                identity.node_id.as_slice(),
                identity.endpoint_id.as_slice(),
                i64::try_from(identity.owner_epoch).map_err(|_| AdapterError::InvalidRequest)?,
                record.record_hmac.as_slice(),
                now,
            ],
        )
        .map_err(sqlite_io)?;
    Ok(())
}
