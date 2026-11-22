use super::super::{
    AdapterError, ArtifactLeaseIdentity, Connection, MAX_ARTIFACT_BYTES, OptionalExtension,
    TransactionBehavior, Uuid, params,
};
use super::hmac::{authenticate_artifact_record, verify_mac};
use super::owner_fence::{ValidatedOwnerFence, enforce_owner_fence, validate_owner_fence};
use super::{EffectStore, effect_now, fixed_blob, sqlite_io};

#[derive(Debug)]
pub(crate) struct ArtifactRecord {
    pub(crate) artifact_id: [u8; 16],
    pub(crate) certificate_id: [u8; 16],
    pub(crate) certificate_version: u64,
    pub(crate) operation_id: [u8; 16],
    pub(crate) content_sha256: [u8; 32],
    pub(crate) content_size: u64,
    pub(crate) artifact_expires_at: i64,
    pub(crate) state: &'static str,
    pub(crate) active_grant_id: Option<[u8; 16]>,
    pub(crate) active_subject: String,
    pub(crate) grant_expires_at: Option<i64>,
    pub(crate) record_hmac: [u8; 32],
}

#[derive(Clone, Copy)]
struct ValidatedArtifactIdentity<'a> {
    artifact_id: &'a [u8; 16],
    certificate_id: &'a [u8; 16],
    certificate_version: u64,
    operation_id: &'a [u8; 16],
    authorized_subject: &'a str,
    max_bytes: u64,
    grant_id: &'a [u8; 16],
    expires_at: i64,
    owner_fence: ValidatedOwnerFence<'a>,
}

impl EffectStore {
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn register_artifact(
        &mut self,
        artifact_id: &[u8],
        certificate_id: &[u8],
        certificate_version: u64,
        operation_id: &[u8],
        content_sha256: &[u8],
        content_size: u64,
        artifact_expires_at: i64,
    ) -> Result<(), AdapterError> {
        let now = effect_now()?;
        let artifact_id: &[u8; 16] = artifact_id
            .try_into()
            .map_err(|_| AdapterError::InvalidRequest)?;
        let certificate_id: &[u8; 16] = certificate_id
            .try_into()
            .map_err(|_| AdapterError::InvalidRequest)?;
        let operation_id: &[u8; 16] = operation_id
            .try_into()
            .map_err(|_| AdapterError::InvalidRequest)?;
        let digest: [u8; 32] = content_sha256
            .try_into()
            .map_err(|_| AdapterError::InvalidRequest)?;
        if [artifact_id, certificate_id, operation_id]
            .into_iter()
            .any(|value| Uuid::from_bytes(*value).get_version_num() != 7)
            || certificate_version == 0
            || content_size == 0
            || content_size > MAX_ARTIFACT_BYTES as u64
            || artifact_expires_at <= now
        {
            return Err(AdapterError::InvalidRequest);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let mut record =
            query_artifact(&transaction, artifact_id)?.ok_or(AdapterError::InvalidRequest)?;
        validate_artifact_record(&self.hmac_key, &record)?;
        if record.certificate_id != *certificate_id
            || record.certificate_version != certificate_version
            || record.operation_id != *operation_id
            || record.artifact_expires_at != artifact_expires_at
        {
            return Err(AdapterError::InvalidRequest);
        }
        if matches!(record.state, "available" | "leased") {
            if record.content_sha256 != digest || record.content_size != content_size {
                return Err(AdapterError::InvalidRequest);
            }
            transaction.commit().map_err(sqlite_io)?;
            return Ok(());
        }
        if record.state != "prepared" {
            return Err(AdapterError::InvalidRequest);
        }
        record.content_sha256 = digest;
        record.content_size = content_size;
        record.state = "available";
        record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
        let changed = transaction
            .execute(
                "UPDATE certificate_artifacts SET content_sha256=?2,content_size=?3,state='available',record_hmac=?4,updated_at=?5 WHERE artifact_id=?1 AND state='prepared'",
                params![
                    record.artifact_id.as_slice(),
                    record.content_sha256.as_slice(),
                    i64::try_from(record.content_size).map_err(|_| AdapterError::InvalidRequest)?,
                    record.record_hmac.as_slice(),
                    now,
                ],
            )
            .map_err(sqlite_io)?;
        if changed != 1 {
            return Err(AdapterError::Unavailable);
        }
        transaction.commit().map_err(sqlite_io)
    }

    pub(crate) fn prepare_artifact(
        &mut self,
        artifact_id: &[u8; 16],
        certificate_id: &[u8; 16],
        certificate_version: u64,
        operation_id: &[u8; 16],
        artifact_expires_at: i64,
    ) -> Result<(), AdapterError> {
        let now = effect_now()?;
        if certificate_version == 0 || artifact_expires_at <= now {
            return Err(AdapterError::InvalidRequest);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        if let Some(existing) = query_artifact(&transaction, artifact_id)? {
            validate_artifact_record(&self.hmac_key, &existing)?;
            if existing.certificate_id != *certificate_id
                || existing.certificate_version != certificate_version
                || existing.operation_id != *operation_id
                || existing.artifact_expires_at != artifact_expires_at
                || !matches!(existing.state, "prepared" | "available" | "leased")
            {
                return Err(AdapterError::InvalidRequest);
            }
            transaction.commit().map_err(sqlite_io)?;
            return Ok(());
        }
        let mut record = ArtifactRecord {
            artifact_id: *artifact_id,
            certificate_id: *certificate_id,
            certificate_version,
            operation_id: *operation_id,
            content_sha256: [0; 32],
            content_size: 0,
            artifact_expires_at,
            state: "prepared",
            active_grant_id: None,
            active_subject: String::new(),
            grant_expires_at: None,
            record_hmac: [0; 32],
        };
        record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
        transaction
            .execute(
                "INSERT INTO certificate_artifacts(artifact_id,certificate_id,certificate_version,operation_id,content_sha256,content_size,artifact_expires_at,state,active_grant_id,active_subject,grant_expires_at,record_hmac,updated_at)
                 VALUES(?1,?2,?3,?4,?5,0,?6,'prepared',NULL,'',NULL,?7,?8)",
                params![record.artifact_id.as_slice(), record.certificate_id.as_slice(), i64::try_from(record.certificate_version).map_err(|_| AdapterError::InvalidRequest)?, record.operation_id.as_slice(), record.content_sha256.as_slice(), record.artifact_expires_at, record.record_hmac.as_slice(), now],
            )
            .map_err(sqlite_io)?;
        transaction.commit().map_err(sqlite_io)
    }

    pub(crate) fn artifact_is_registered(
        &self,
        artifact_id: &[u8; 16],
        certificate_id: &[u8; 16],
        certificate_version: u64,
        operation_id: &[u8; 16],
        artifact_expires_at: i64,
    ) -> Result<bool, AdapterError> {
        let Some(record) = query_artifact(&self.connection, artifact_id)? else {
            return Ok(false);
        };
        validate_artifact_record(&self.hmac_key, &record)?;
        if record.certificate_id != *certificate_id
            || record.certificate_version != certificate_version
            || record.operation_id != *operation_id
            || record.artifact_expires_at != artifact_expires_at
            || !matches!(record.state, "prepared" | "available" | "leased")
        {
            return Err(AdapterError::InvalidRequest);
        }
        Ok(record.state != "prepared")
    }

    pub(crate) fn lease_artifact(
        &mut self,
        identity: ArtifactLeaseIdentity<'_>,
    ) -> Result<ArtifactRecord, AdapterError> {
        let now = effect_now()?;
        let identity = validate_artifact_identity(identity, now)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        enforce_owner_fence(&transaction, &self.hmac_key, identity.owner_fence, now)?;
        let mut record = query_artifact(&transaction, identity.artifact_id)?
            .ok_or(AdapterError::InvalidRequest)?;
        validate_artifact_record(&self.hmac_key, &record)?;
        if record.certificate_id != *identity.certificate_id
            || record.certificate_version != identity.certificate_version
            || record.operation_id != *identity.operation_id
            || record.content_size > identity.max_bytes
            || record.artifact_expires_at <= now
            || !matches!(record.state, "available" | "leased")
        {
            return Err(AdapterError::InvalidRequest);
        }
        if record.state == "leased" {
            if record.active_grant_id == Some(*identity.grant_id) {
                return Err(AdapterError::InvalidRequest);
            }
            if record.grant_expires_at.is_some_and(|expiry| expiry > now) {
                return Err(AdapterError::Unavailable);
            }
        }
        record.state = "leased";
        record.active_grant_id = Some(*identity.grant_id);
        identity
            .authorized_subject
            .clone_into(&mut record.active_subject);
        record.grant_expires_at = Some(identity.expires_at);
        record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
        let changed = transaction
            .execute(
                "UPDATE certificate_artifacts SET state='leased',active_grant_id=?2,active_subject=?3,grant_expires_at=?4,record_hmac=?5,updated_at=?6 WHERE artifact_id=?1",
                params![record.artifact_id.as_slice(), identity.grant_id.as_slice(), identity.authorized_subject, identity.expires_at, record.record_hmac.as_slice(), now],
            )
            .map_err(sqlite_io)?;
        if changed != 1 {
            return Err(AdapterError::Unavailable);
        }
        transaction.commit().map_err(sqlite_io)?;
        Ok(record)
    }

    pub(crate) fn consume_artifact(
        &mut self,
        identity: ArtifactLeaseIdentity<'_>,
        content_sha256: &[u8],
        content_size: u64,
    ) -> Result<(), AdapterError> {
        let now = effect_now()?;
        let identity = validate_artifact_identity(identity, now)?;
        let digest: [u8; 32] = content_sha256
            .try_into()
            .map_err(|_| AdapterError::InvalidRequest)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        enforce_owner_fence(&transaction, &self.hmac_key, identity.owner_fence, now)?;
        let mut record = query_artifact(&transaction, identity.artifact_id)?
            .ok_or(AdapterError::InvalidRequest)?;
        validate_artifact_record(&self.hmac_key, &record)?;
        let exact_identity = record.active_grant_id == Some(*identity.grant_id)
            && record.active_subject == identity.authorized_subject
            && record.grant_expires_at == Some(identity.expires_at)
            && record.certificate_id == *identity.certificate_id
            && record.certificate_version == identity.certificate_version
            && record.operation_id == *identity.operation_id
            && record.content_sha256 == digest
            && record.content_size == content_size;
        if record.state == "consumed" && exact_identity {
            transaction.commit().map_err(sqlite_io)?;
            return Ok(());
        }
        if record.state != "leased" || !exact_identity {
            return Err(AdapterError::InvalidRequest);
        }
        record.state = "consumed";
        record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
        let changed = transaction
            .execute(
                "UPDATE certificate_artifacts SET state='consumed',record_hmac=?2,updated_at=?3 WHERE artifact_id=?1 AND state='leased'",
                params![record.artifact_id.as_slice(), record.record_hmac.as_slice(), now],
            )
            .map_err(sqlite_io)?;
        if changed != 1 {
            return Err(AdapterError::Unavailable);
        }
        transaction.commit().map_err(sqlite_io)
    }

    pub(crate) fn confirm_artifact_consumed(
        &mut self,
        identity: ArtifactLeaseIdentity<'_>,
        content_sha256: &[u8],
        content_size: u64,
    ) -> Result<bool, AdapterError> {
        let now = effect_now()?;
        let identity = validate_artifact_identity_for_confirmation(identity)?;
        let digest: [u8; 32] = content_sha256
            .try_into()
            .map_err(|_| AdapterError::InvalidRequest)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        enforce_owner_fence(&transaction, &self.hmac_key, identity.owner_fence, now)?;
        let record = query_artifact(&transaction, identity.artifact_id)?
            .ok_or(AdapterError::InvalidRequest)?;
        validate_artifact_record(&self.hmac_key, &record)?;
        let same_artifact = record.certificate_id == *identity.certificate_id
            && record.certificate_version == identity.certificate_version
            && record.operation_id == *identity.operation_id
            && record.content_sha256 == digest
            && record.content_size == content_size;
        if !same_artifact {
            return Err(AdapterError::InvalidRequest);
        }
        let exact_grant = record.active_grant_id == Some(*identity.grant_id)
            && record.active_subject == identity.authorized_subject
            && record.grant_expires_at == Some(identity.expires_at);
        let consumed = match record.state {
            "consumed" if exact_grant => Ok(true),
            "leased" if exact_grant => Ok(false),
            "available" | "expired" | "revoked" => Ok(false),
            _ => Err(AdapterError::InvalidRequest),
        }?;
        transaction.commit().map_err(sqlite_io)?;
        Ok(consumed)
    }

    pub(crate) fn revoke_certificate_artifacts(
        &mut self,
        certificate_id: &[u8; 16],
    ) -> Result<Vec<[u8; 16]>, AdapterError> {
        let now = effect_now()?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let records = query_certificate_artifact_records(&transaction, certificate_id)?;
        let mut artifact_ids = Vec::with_capacity(records.len());
        let mut update_stmt = transaction
            .prepare_cached(
                "UPDATE certificate_artifacts SET state='revoked',record_hmac=?2,updated_at=?3 WHERE artifact_id=?1",
            )
            .map_err(sqlite_io)?;
        for mut record in records {
            validate_artifact_record(&self.hmac_key, &record)?;
            artifact_ids.push(record.artifact_id);
            if !matches!(record.state, "consumed" | "revoked") {
                record.state = "revoked";
                record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
                update_stmt
                    .execute(params![
                        record.artifact_id.as_slice(),
                        record.record_hmac.as_slice(),
                        now
                    ])
                    .map_err(sqlite_io)?;
            }
        }
        drop(update_stmt);
        transaction.commit().map_err(sqlite_io)?;
        Ok(artifact_ids)
    }

    pub(crate) fn reconcile_artifacts(&mut self) -> Result<Vec<[u8; 16]>, AdapterError> {
        let now = effect_now()?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let records = query_all_artifact_records(&transaction)?;
        let mut terminal = Vec::new();
        let mut update_stmt = transaction
            .prepare_cached(
                "UPDATE certificate_artifacts SET state=?2,active_grant_id=?3,active_subject=?4,grant_expires_at=?5,record_hmac=?6,updated_at=?7 WHERE artifact_id=?1",
            )
            .map_err(sqlite_io)?;
        for mut record in records {
            validate_artifact_record(&self.hmac_key, &record)?;
            let mut changed = false;
            if record.artifact_expires_at <= now
                && matches!(record.state, "prepared" | "available" | "leased")
            {
                record.state = "expired";
                changed = true;
            } else if record.state == "leased"
                && record.grant_expires_at.is_some_and(|expiry| expiry <= now)
            {
                record.state = "available";
                record.active_grant_id = None;
                record.active_subject.clear();
                record.grant_expires_at = None;
                changed = true;
            }
            if matches!(record.state, "consumed" | "expired" | "revoked") {
                terminal.push(record.artifact_id);
            }
            if changed {
                record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
                update_stmt
                    .execute(params![
                        record.artifact_id.as_slice(),
                        record.state,
                        record.active_grant_id.as_ref().map(<[u8; 16]>::as_slice),
                        record.active_subject,
                        record.grant_expires_at,
                        record.record_hmac.as_slice(),
                        now
                    ])
                    .map_err(sqlite_io)?;
            }
        }
        drop(update_stmt);
        transaction.commit().map_err(sqlite_io)?;
        Ok(terminal)
    }

    pub(crate) fn tracked_artifact_ids(&self) -> Result<Vec<[u8; 16]>, AdapterError> {
        let mut statement = self
            .connection
            .prepare_cached("SELECT artifact_id FROM certificate_artifacts ORDER BY artifact_id")
            .map_err(sqlite_io)?;
        let rows = statement
            .query_map([], |row| fixed_blob(row.get(0)?))
            .map_err(sqlite_io)?;
        rows.collect::<Result<Vec<_>, _>>().map_err(sqlite_io)
    }
}

fn validate_artifact_identity(
    identity: ArtifactLeaseIdentity<'_>,
    now: i64,
) -> Result<ValidatedArtifactIdentity<'_>, AdapterError> {
    let validated = validate_artifact_identity_for_confirmation(identity)?;
    if validated.expires_at <= now {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(validated)
}

fn validate_artifact_identity_for_confirmation(
    identity: ArtifactLeaseIdentity<'_>,
) -> Result<ValidatedArtifactIdentity<'_>, AdapterError> {
    let artifact_id: &[u8; 16] = identity
        .artifact_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let certificate_id: &[u8; 16] = identity
        .certificate_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let operation_id: &[u8; 16] = identity
        .operation_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let grant_id: &[u8; 16] = identity
        .grant_id
        .try_into()
        .map_err(|_| AdapterError::InvalidRequest)?;
    let owner_fence = validate_owner_fence(identity.owner_fence)?;
    if [artifact_id, certificate_id, operation_id, grant_id]
        .into_iter()
        .any(|value| Uuid::from_bytes(*value).get_version_num() != 7)
        || identity.certificate_version == 0
        || identity.authorized_subject.is_empty()
        || identity.authorized_subject.len() > 256
        || identity.max_bytes == 0
        || identity.max_bytes > MAX_ARTIFACT_BYTES as u64
        || identity.expires_at_unix_seconds <= 0
    {
        return Err(AdapterError::InvalidRequest);
    }
    Ok(ValidatedArtifactIdentity {
        artifact_id,
        certificate_id,
        certificate_version: identity.certificate_version,
        operation_id,
        authorized_subject: identity.authorized_subject,
        max_bytes: identity.max_bytes,
        grant_id,
        expires_at: identity.expires_at_unix_seconds,
        owner_fence,
    })
}

fn decode_artifact_row(row: &rusqlite::Row<'_>) -> Result<ArtifactRecord, rusqlite::Error> {
    let certificate_version: i64 = row.get(2)?;
    let content_size: i64 = row.get(5)?;
    let active_grant: Option<Vec<u8>> = row.get(8)?;
    Ok(ArtifactRecord {
        artifact_id: fixed_blob(row.get(0)?)?,
        certificate_id: fixed_blob(row.get(1)?)?,
        certificate_version: u64::try_from(certificate_version)
            .map_err(|_| rusqlite::Error::InvalidQuery)?,
        operation_id: fixed_blob(row.get(3)?)?,
        content_sha256: fixed_blob(row.get(4)?)?,
        content_size: u64::try_from(content_size).map_err(|_| rusqlite::Error::InvalidQuery)?,
        artifact_expires_at: row.get(6)?,
        state: match row.get::<_, String>(7)?.as_str() {
            "prepared" => "prepared",
            "available" => "available",
            "leased" => "leased",
            "consumed" => "consumed",
            "expired" => "expired",
            "revoked" => "revoked",
            _ => return Err(rusqlite::Error::InvalidQuery),
        },
        active_grant_id: active_grant.map(fixed_blob).transpose()?,
        active_subject: row.get(9)?,
        grant_expires_at: row.get(10)?,
        record_hmac: fixed_blob(row.get(11)?)?,
    })
}

pub(crate) fn query_artifact(
    connection: &Connection,
    artifact_id: &[u8; 16],
) -> Result<Option<ArtifactRecord>, AdapterError> {
    let mut statement = connection
        .prepare_cached(
            "SELECT artifact_id,certificate_id,certificate_version,operation_id,content_sha256,content_size,artifact_expires_at,state,active_grant_id,active_subject,grant_expires_at,record_hmac FROM certificate_artifacts WHERE artifact_id=?1",
        )
        .map_err(sqlite_io)?;
    statement
        .query_row([artifact_id.as_slice()], decode_artifact_row)
        .optional()
        .map_err(sqlite_io)
}

fn query_certificate_artifact_records(
    connection: &Connection,
    certificate_id: &[u8; 16],
) -> Result<Vec<ArtifactRecord>, AdapterError> {
    let mut statement = connection
        .prepare_cached(
            "SELECT artifact_id,certificate_id,certificate_version,operation_id,content_sha256,content_size,artifact_expires_at,state,active_grant_id,active_subject,grant_expires_at,record_hmac FROM certificate_artifacts WHERE certificate_id=?1",
        )
        .map_err(sqlite_io)?;
    let rows = statement
        .query_map([certificate_id.as_slice()], decode_artifact_row)
        .map_err(sqlite_io)?;
    rows.collect::<Result<Vec<_>, _>>().map_err(sqlite_io)
}

fn query_all_artifact_records(
    connection: &Connection,
) -> Result<Vec<ArtifactRecord>, AdapterError> {
    let mut statement = connection
        .prepare_cached(
            "SELECT artifact_id,certificate_id,certificate_version,operation_id,content_sha256,content_size,artifact_expires_at,state,active_grant_id,active_subject,grant_expires_at,record_hmac FROM certificate_artifacts ORDER BY artifact_id",
        )
        .map_err(sqlite_io)?;
    let rows = statement
        .query_map([], decode_artifact_row)
        .map_err(sqlite_io)?;
    rows.collect::<Result<Vec<_>, _>>().map_err(sqlite_io)
}

fn validate_artifact_record(key: &[u8; 32], record: &ArtifactRecord) -> Result<(), AdapterError> {
    if record.content_size > MAX_ARTIFACT_BYTES as u64
        || (record.state == "prepared" && record.content_size != 0)
        || (!matches!(record.state, "prepared" | "expired" | "revoked") && record.content_size == 0)
        || !matches!(
            record.state,
            "prepared" | "available" | "leased" | "consumed" | "expired" | "revoked"
        )
        || (record.state == "leased"
            && (record.active_grant_id.is_none()
                || record.active_subject.is_empty()
                || record.grant_expires_at.is_none()))
        || !verify_mac(
            &authenticate_artifact_record(key, record),
            &record.record_hmac,
        )
    {
        return Err(AdapterError::InvalidResource);
    }
    Ok(())
}
