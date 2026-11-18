use super::{
    AdapterError, ArtifactLeaseIdentity, AuthorizedEffectDecision, AuthorizedEffectIdentity,
    Connection, DesiredEffectObservation, DesiredEffectState, Digest, EFFECT_STORE_KEY_BYTES,
    EffectIdentity, FixedResources, MAX_ARTIFACT_BYTES, MAX_EFFECT_RECORDS, MAX_FRAME_BYTES,
    OpenFlags, OpenOptionsExt, OptionalExtension, Path, PermissionsExt, RngCore, Sha256,
    StdOpenOptions, SystemTime, TransactionBehavior, UNIX_EPOCH, Uuid, Zeroizing, io, params,
    validate_name,
};

pub(super) fn validate_effect_identity(
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
pub(super) enum PrepareEffect {
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

#[derive(Debug)]
pub(super) struct AuthorizedEffectRecord {
    command_id: [u8; 16],
    idempotency_key: [u8; 16],
    node_id: [u8; 16],
    operation_id: [u8; 16],
    payload_sha256: [u8; 32],
    action: String,
    authorization_revision: u64,
    effect_kind: String,
    resource_key: String,
    effect_revision: u64,
    pub(super) expires_at: i64,
    state: &'static str,
    delivery_mode: i64,
    response: Vec<u8>,
    pub(super) record_hmac: [u8; 32],
}

#[derive(Debug)]
pub(super) struct ArtifactRecord {
    pub(super) artifact_id: [u8; 16],
    certificate_id: [u8; 16],
    certificate_version: u64,
    operation_id: [u8; 16],
    pub(super) content_sha256: [u8; 32],
    pub(super) content_size: u64,
    artifact_expires_at: i64,
    state: &'static str,
    active_grant_id: Option<[u8; 16]>,
    active_subject: String,
    pub(super) grant_expires_at: Option<i64>,
    pub(super) active_offset: u64,
    pub(super) record_hmac: [u8; 32],
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

#[derive(Debug)]
pub(super) struct EffectStore {
    pub(super) connection: Connection,
    pub(super) hmac_key: Zeroizing<[u8; EFFECT_STORE_KEY_BYTES]>,
}

impl EffectStore {
    pub(super) fn open_for_mutation(resources: &FixedResources) -> Result<Self, AdapterError> {
        Self::open(resources, true)
    }

    pub(super) fn open_existing(resources: &FixedResources) -> Result<Self, AdapterError> {
        Self::open(resources, false)
    }

    #[allow(clippy::too_many_lines)]
    fn open(resources: &FixedResources, allow_initialize: bool) -> Result<Self, AdapterError> {
        let parent = resources
            .effect_store
            .parent()
            .ok_or(AdapterError::InvalidResource)?;
        reject_symlink(&resources.effect_store)?;
        reject_symlink(&resources.effect_store_key)?;
        let database_exists = resources.effect_store.exists();
        let key_exists = resources.effect_store_key.exists();
        let initialize = match (database_exists, key_exists, allow_initialize) {
            (false, false, true) => true,
            (true, true, _) => false,
            _ => return Err(AdapterError::Unavailable),
        };
        if initialize {
            std::fs::create_dir_all(parent).map_err(AdapterError::Io)?;
            std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700))
                .map_err(AdapterError::Io)?;
        }
        let hmac_key = if initialize {
            load_or_create_effect_key(&resources.effect_store_key)?
        } else {
            load_effect_key(&resources.effect_store_key)?
        };
        let mut flags = OpenFlags::SQLITE_OPEN_READ_WRITE | OpenFlags::SQLITE_OPEN_NO_MUTEX;
        if initialize {
            flags |= OpenFlags::SQLITE_OPEN_CREATE;
        }
        let connection =
            Connection::open_with_flags(&resources.effect_store, flags).map_err(sqlite_io)?;
        if initialize {
            std::fs::set_permissions(
                &resources.effect_store,
                std::fs::Permissions::from_mode(0o600),
            )
            .map_err(AdapterError::Io)?;
        } else {
            let metadata =
                std::fs::symlink_metadata(&resources.effect_store).map_err(AdapterError::Io)?;
            if !metadata.file_type().is_file() || metadata.permissions().mode() & 0o077 != 0 {
                return Err(AdapterError::InvalidResource);
            }
        }
        connection
            .execute_batch(
                "PRAGMA journal_mode=DELETE;
                 PRAGMA synchronous=FULL;
                 PRAGMA foreign_keys=ON;
                 PRAGMA trusted_schema=OFF;",
            )
            .map_err(sqlite_io)?;
        if initialize {
            connection
                .execute_batch(
                    "CREATE TABLE effect_store_identity (
                   singleton INTEGER PRIMARY KEY CHECK(singleton=1),
                   store_id BLOB NOT NULL CHECK(length(store_id)=16),
                   identity_hmac BLOB NOT NULL CHECK(length(identity_hmac)=32)
                 ) STRICT;
                 CREATE TABLE desired_effects (
                   mutation_kind TEXT NOT NULL,
                   resource_key TEXT NOT NULL,
                   revision INTEGER NOT NULL CHECK(revision > 0),
                   command_id BLOB NOT NULL CHECK(length(command_id)=16),
                   idempotency_key BLOB NOT NULL CHECK(length(idempotency_key)=16),
                   payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32),
                   expires_at INTEGER NOT NULL,
                   state TEXT NOT NULL CHECK(state IN ('prepared','applied','absent')),
                   before_hmac BLOB NOT NULL CHECK(length(before_hmac)=32),
                   after_hmac BLOB NOT NULL CHECK(length(after_hmac)=32),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL,
                   PRIMARY KEY(mutation_kind, resource_key)
                 ) STRICT;
                 CREATE TABLE authorized_effects (
                   command_id BLOB PRIMARY KEY CHECK(length(command_id)=16),
                   idempotency_key BLOB NOT NULL UNIQUE CHECK(length(idempotency_key)=16),
                   node_id BLOB NOT NULL CHECK(length(node_id)=16),
                   operation_id BLOB NOT NULL CHECK(length(operation_id)=16),
                   payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32),
                   action TEXT NOT NULL,
                   authorization_revision INTEGER NOT NULL CHECK(authorization_revision > 0),
                   effect_kind TEXT NOT NULL,
                   resource_key TEXT NOT NULL,
                   effect_revision INTEGER NOT NULL CHECK(effect_revision > 0),
                   expires_at INTEGER NOT NULL,
                   state TEXT NOT NULL CHECK(state IN ('prepared','applied')),
                   delivery_mode INTEGER NOT NULL CHECK(delivery_mode IN (1,3)),
                   response BLOB NOT NULL CHECK(length(response) <= 393216),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL
                 ) STRICT;",
                )
                .map_err(sqlite_io)?;
            let store_id = *Uuid::now_v7().as_bytes();
            let identity_hmac = authenticate_store_identity(&hmac_key, &store_id);
            connection
                .execute(
                    "INSERT INTO effect_store_identity(singleton,store_id,identity_hmac) VALUES(1,?1,?2)",
                    params![store_id.as_slice(), identity_hmac.as_slice()],
                )
                .map_err(sqlite_io)?;
        }
        let store = Self {
            connection,
            hmac_key,
        };
        store.migrate_schema()?;
        store.ensure_authorized_schema()?;
        store.ensure_artifact_schema()?;
        store.validate_identity()?;
        Ok(store)
    }

    fn migrate_schema(&self) -> Result<(), AdapterError> {
        let schema: String = self
            .connection
            .query_row(
                "SELECT sql FROM sqlite_master WHERE type='table' AND name='desired_effects'",
                [],
                |row| row.get(0),
            )
            .map_err(sqlite_io)?;
        if schema.contains("'absent'") {
            return Ok(());
        }
        self.connection
            .execute_batch(
                "BEGIN IMMEDIATE;
                 ALTER TABLE desired_effects RENAME TO desired_effects_legacy;
                 CREATE TABLE desired_effects (
                   mutation_kind TEXT NOT NULL,
                   resource_key TEXT NOT NULL,
                   revision INTEGER NOT NULL CHECK(revision > 0),
                   command_id BLOB NOT NULL CHECK(length(command_id)=16),
                   idempotency_key BLOB NOT NULL CHECK(length(idempotency_key)=16),
                   payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32),
                   expires_at INTEGER NOT NULL,
                   state TEXT NOT NULL CHECK(state IN ('prepared','applied','absent')),
                   before_hmac BLOB NOT NULL CHECK(length(before_hmac)=32),
                   after_hmac BLOB NOT NULL CHECK(length(after_hmac)=32),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL,
                   PRIMARY KEY(mutation_kind,resource_key)
                 ) STRICT;
                 INSERT INTO desired_effects
                    SELECT * FROM desired_effects_legacy;
                 DROP TABLE desired_effects_legacy;
                 COMMIT;",
            )
            .map_err(sqlite_io)
    }

    fn ensure_authorized_schema(&self) -> Result<(), AdapterError> {
        self.connection
            .execute_batch(
                "CREATE TABLE IF NOT EXISTS authorized_effects (
                   command_id BLOB PRIMARY KEY CHECK(length(command_id)=16),
                   idempotency_key BLOB NOT NULL UNIQUE CHECK(length(idempotency_key)=16),
                   node_id BLOB NOT NULL CHECK(length(node_id)=16),
                   operation_id BLOB NOT NULL CHECK(length(operation_id)=16),
                   payload_sha256 BLOB NOT NULL CHECK(length(payload_sha256)=32),
                   action TEXT NOT NULL,
                   authorization_revision INTEGER NOT NULL CHECK(authorization_revision > 0),
                   effect_kind TEXT NOT NULL,
                   resource_key TEXT NOT NULL,
                   effect_revision INTEGER NOT NULL CHECK(effect_revision > 0),
                   expires_at INTEGER NOT NULL,
                   state TEXT NOT NULL CHECK(state IN ('prepared','applied')),
                   delivery_mode INTEGER NOT NULL CHECK(delivery_mode IN (1,3)),
                   response BLOB NOT NULL CHECK(length(response) <= 393216),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL
                 ) STRICT;",
            )
            .map_err(sqlite_io)
    }

    fn ensure_artifact_schema(&self) -> Result<(), AdapterError> {
        self.connection
            .execute_batch(
                "CREATE TABLE IF NOT EXISTS certificate_artifacts (
                   artifact_id BLOB PRIMARY KEY CHECK(length(artifact_id)=16),
                   certificate_id BLOB NOT NULL CHECK(length(certificate_id)=16),
                   certificate_version INTEGER NOT NULL CHECK(certificate_version > 0),
                   operation_id BLOB NOT NULL CHECK(length(operation_id)=16),
                   content_sha256 BLOB NOT NULL CHECK(length(content_sha256)=32),
                   content_size INTEGER NOT NULL CHECK(content_size BETWEEN 0 AND 67108864),
                   artifact_expires_at INTEGER NOT NULL,
                   state TEXT NOT NULL CHECK(state IN ('prepared','available','leased','consumed','expired','revoked')),
                   active_grant_id BLOB CHECK(active_grant_id IS NULL OR length(active_grant_id)=16),
                   active_subject TEXT NOT NULL CHECK(length(active_subject) <= 256),
                   grant_expires_at INTEGER,
                   active_offset INTEGER NOT NULL CHECK(active_offset BETWEEN 0 AND content_size),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL,
                   CHECK((state='leased' AND active_grant_id IS NOT NULL AND grant_expires_at IS NOT NULL AND length(active_subject)>0) OR state<>'leased'),
                   CHECK((state='prepared' AND content_size=0) OR (state IN ('expired','revoked')) OR (state NOT IN ('prepared','expired','revoked') AND content_size>=1))
                 ) STRICT;
                 CREATE INDEX IF NOT EXISTS certificate_artifacts_certificate_idx
                   ON certificate_artifacts(certificate_id);",
            )
            .map_err(sqlite_io)
    }

    #[allow(clippy::too_many_arguments)]
    pub(super) fn register_artifact(
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

    pub(super) fn prepare_artifact(
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
            active_offset: 0,
            record_hmac: [0; 32],
        };
        record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
        transaction
            .execute(
                "INSERT INTO certificate_artifacts(artifact_id,certificate_id,certificate_version,operation_id,content_sha256,content_size,artifact_expires_at,state,active_grant_id,active_subject,grant_expires_at,active_offset,record_hmac,updated_at)
                 VALUES(?1,?2,?3,?4,?5,0,?6,'prepared',NULL,'',NULL,0,?7,?8)",
                params![record.artifact_id.as_slice(), record.certificate_id.as_slice(), i64::try_from(record.certificate_version).map_err(|_| AdapterError::InvalidRequest)?, record.operation_id.as_slice(), record.content_sha256.as_slice(), record.artifact_expires_at, record.record_hmac.as_slice(), now],
            )
            .map_err(sqlite_io)?;
        transaction.commit().map_err(sqlite_io)
    }

    pub(super) fn artifact_is_registered(
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

    pub(super) fn lease_artifact(
        &mut self,
        identity: ArtifactLeaseIdentity<'_>,
    ) -> Result<ArtifactRecord, AdapterError> {
        let now = effect_now()?;
        let identity = validate_artifact_identity(identity, now)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
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
        if record.state == "leased"
            && record.active_grant_id != Some(*identity.grant_id)
            && record.grant_expires_at.is_some_and(|expiry| expiry > now)
        {
            return Err(AdapterError::Unavailable);
        }
        if record.state == "leased"
            && record.active_grant_id == Some(*identity.grant_id)
            && (record.active_subject != identity.authorized_subject
                || record.grant_expires_at != Some(identity.expires_at))
        {
            return Err(AdapterError::InvalidRequest);
        }
        if record.state != "leased" || record.active_grant_id != Some(*identity.grant_id) {
            record.state = "leased";
            record.active_grant_id = Some(*identity.grant_id);
            identity
                .authorized_subject
                .clone_into(&mut record.active_subject);
            record.grant_expires_at = Some(identity.expires_at);
            record.active_offset = 0;
            record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
            let changed = transaction
                .execute(
                    "UPDATE certificate_artifacts SET state='leased',active_grant_id=?2,active_subject=?3,grant_expires_at=?4,active_offset=0,record_hmac=?5,updated_at=?6 WHERE artifact_id=?1",
                    params![record.artifact_id.as_slice(), identity.grant_id.as_slice(), identity.authorized_subject, identity.expires_at, record.record_hmac.as_slice(), now],
                )
                .map_err(sqlite_io)?;
            if changed != 1 {
                return Err(AdapterError::Unavailable);
            }
        }
        transaction.commit().map_err(sqlite_io)?;
        Ok(record)
    }

    pub(super) fn advance_artifact(
        &mut self,
        identity: ArtifactLeaseIdentity<'_>,
        expected_offset: u64,
        next_offset: u64,
    ) -> Result<(), AdapterError> {
        let now = effect_now()?;
        let identity = validate_artifact_identity(identity, now)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let mut record = query_artifact(&transaction, identity.artifact_id)?
            .ok_or(AdapterError::InvalidRequest)?;
        validate_artifact_record(&self.hmac_key, &record)?;
        if record.state != "leased"
            || record.active_grant_id != Some(*identity.grant_id)
            || record.active_subject != identity.authorized_subject
            || record.grant_expires_at != Some(identity.expires_at)
            || record.certificate_id != *identity.certificate_id
            || record.certificate_version != identity.certificate_version
            || record.operation_id != *identity.operation_id
            || record.active_offset != expected_offset
            || next_offset <= expected_offset
            || next_offset > record.content_size
        {
            return Err(AdapterError::InvalidRequest);
        }
        record.active_offset = next_offset;
        record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
        let changed = transaction
            .execute(
                "UPDATE certificate_artifacts SET active_offset=?2,record_hmac=?3,updated_at=?4 WHERE artifact_id=?1 AND state='leased' AND active_offset=?5",
                params![record.artifact_id.as_slice(), i64::try_from(next_offset).map_err(|_| AdapterError::InvalidRequest)?, record.record_hmac.as_slice(), now, i64::try_from(expected_offset).map_err(|_| AdapterError::InvalidRequest)?],
            )
            .map_err(sqlite_io)?;
        if changed != 1 {
            return Err(AdapterError::Unavailable);
        }
        transaction.commit().map_err(sqlite_io)
    }

    pub(super) fn consume_artifact(
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
            && record.content_size == content_size
            && record.active_offset == record.content_size;
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

    pub(super) fn confirm_artifact_consumed(
        &self,
        identity: ArtifactLeaseIdentity<'_>,
        content_sha256: &[u8],
        content_size: u64,
    ) -> Result<bool, AdapterError> {
        let identity = validate_artifact_identity_for_confirmation(identity)?;
        let digest: [u8; 32] = content_sha256
            .try_into()
            .map_err(|_| AdapterError::InvalidRequest)?;
        let record = query_artifact(&self.connection, identity.artifact_id)?
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
            && record.grant_expires_at == Some(identity.expires_at)
            && record.active_offset == record.content_size;
        match record.state {
            "consumed" if exact_grant => Ok(true),
            "leased" if exact_grant => Ok(false),
            "available" | "expired" | "revoked" => Ok(false),
            _ => Err(AdapterError::InvalidRequest),
        }
    }

    pub(super) fn revoke_certificate_artifacts(
        &mut self,
        certificate_id: &[u8; 16],
    ) -> Result<Vec<[u8; 16]>, AdapterError> {
        let now = effect_now()?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let artifact_ids = query_certificate_artifacts(&transaction, certificate_id)?;
        for artifact_id in &artifact_ids {
            let mut record =
                query_artifact(&transaction, artifact_id)?.ok_or(AdapterError::InvalidResource)?;
            validate_artifact_record(&self.hmac_key, &record)?;
            if record.state != "consumed" {
                record.state = "revoked";
                record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
                transaction
                    .execute(
                        "UPDATE certificate_artifacts SET state='revoked',record_hmac=?2,updated_at=?3 WHERE artifact_id=?1",
                        params![artifact_id.as_slice(), record.record_hmac.as_slice(), now],
                    )
                    .map_err(sqlite_io)?;
            }
        }
        transaction.commit().map_err(sqlite_io)?;
        Ok(artifact_ids)
    }

    pub(super) fn reconcile_artifacts(&mut self) -> Result<Vec<[u8; 16]>, AdapterError> {
        let now = effect_now()?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
        let artifact_ids = query_all_artifacts(&transaction)?;
        let mut terminal = Vec::new();
        for artifact_id in artifact_ids {
            let mut record =
                query_artifact(&transaction, &artifact_id)?.ok_or(AdapterError::InvalidResource)?;
            validate_artifact_record(&self.hmac_key, &record)?;
            if record.artifact_expires_at <= now
                && matches!(record.state, "prepared" | "available" | "leased")
            {
                record.state = "expired";
            } else if record.state == "leased"
                && record.grant_expires_at.is_some_and(|expiry| expiry <= now)
            {
                record.state = "available";
                record.active_grant_id = None;
                record.active_subject.clear();
                record.grant_expires_at = None;
                record.active_offset = 0;
            }
            if matches!(record.state, "consumed" | "expired" | "revoked") {
                terminal.push(record.artifact_id);
            }
            record.record_hmac = authenticate_artifact_record(&self.hmac_key, &record);
            transaction
                .execute(
                    "UPDATE certificate_artifacts SET state=?2,active_grant_id=?3,active_subject=?4,grant_expires_at=?5,active_offset=?6,record_hmac=?7,updated_at=?8 WHERE artifact_id=?1",
                    params![record.artifact_id.as_slice(), record.state, record.active_grant_id.map(|value| value.to_vec()), record.active_subject, record.grant_expires_at, record.active_offset, record.record_hmac.as_slice(), now],
                )
                .map_err(sqlite_io)?;
        }
        transaction.commit().map_err(sqlite_io)?;
        Ok(terminal)
    }

    pub(super) fn tracked_artifact_ids(&self) -> Result<Vec<[u8; 16]>, AdapterError> {
        query_all_artifacts(&self.connection)
    }

    fn validate_identity(&self) -> Result<(), AdapterError> {
        let (store_id, identity_hmac): (Vec<u8>, Vec<u8>) = self
            .connection
            .query_row(
                "SELECT store_id,identity_hmac FROM effect_store_identity WHERE singleton=1",
                [],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .map_err(sqlite_io)?;
        let store_id: [u8; 16] = store_id
            .try_into()
            .map_err(|_| AdapterError::InvalidResource)?;
        let identity_hmac: [u8; 32] = identity_hmac
            .try_into()
            .map_err(|_| AdapterError::InvalidResource)?;
        if authenticate_store_identity(&self.hmac_key, &store_id) != identity_hmac {
            return Err(AdapterError::InvalidResource);
        }
        Ok(())
    }

    pub(super) fn prepare_authorized(
        &mut self,
        identity: AuthorizedEffectIdentity<'_>,
    ) -> Result<AuthorizedEffectDecision, AdapterError> {
        let now = effect_now()?;
        let identity = validate_authorized_effect(identity, now)?;
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)
            .map_err(sqlite_io)?;
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
            .query_row(
                "SELECT command_id FROM authorized_effects WHERE idempotency_key=?1",
                [identity.idempotency_key.as_slice()],
                |row| row.get(0),
            )
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

    pub(super) fn complete_authorized(
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

    pub(super) fn replay_authorized(
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

    pub(super) fn prepare(
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
                if current_hmac == existing.after_hmac {
                    if mutation_kind == "config_apply" {
                        transaction.commit().map_err(sqlite_io)?;
                        return Ok(PrepareEffect::Proceed);
                    }
                    update_effect_state(&transaction, &self.hmac_key, existing, "applied", now)?;
                    transaction.commit().map_err(sqlite_io)?;
                    return Ok(PrepareEffect::AlreadyApplied);
                }
                if current_hmac != existing.before_hmac {
                    return Err(AdapterError::Unavailable);
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
        let before_hmac = authenticate_file(&self.hmac_key, before);
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

    pub(super) fn mark_applied(
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

    pub(super) fn mark_absent(
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

    pub(super) fn observe(
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
        } else if current_hmac == record.before_hmac && mutation_kind == "config_apply" {
            update_effect_state(&transaction, &self.hmac_key, record, "absent", now)?;
            DesiredEffectState::Absent
        } else if current_hmac == record.before_hmac {
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

pub(super) fn validate_effect_context<'a>(
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

#[derive(Clone, Copy)]
pub(super) struct ValidatedEffectIdentity<'a> {
    pub(super) command_id: &'a [u8; 16],
    pub(super) idempotency_key: &'a [u8; 16],
    pub(super) payload_sha256: &'a [u8; 32],
    pub(super) expires_at: i64,
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
    })
}

pub(super) fn effect_now() -> Result<i64, AdapterError> {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| AdapterError::Unavailable)?
            .as_secs(),
    )
    .map_err(|_| AdapterError::Unavailable)
}

fn load_or_create_effect_key(path: &Path) -> Result<Zeroizing<[u8; 32]>, AdapterError> {
    reject_symlink(path)?;
    if !path.exists() {
        let parent = path.parent().ok_or(AdapterError::InvalidResource)?;
        std::fs::create_dir_all(parent).map_err(AdapterError::Io)?;
        let mut key = Zeroizing::new([0_u8; EFFECT_STORE_KEY_BYTES]);
        rand::rng().fill_bytes(key.as_mut());
        let mut options = StdOpenOptions::new();
        options.write(true).create_new(true).mode(0o600);
        let mut file = options.open(path).map_err(AdapterError::Io)?;
        std::io::Write::write_all(&mut file, key.as_ref()).map_err(AdapterError::Io)?;
        file.sync_all().map_err(AdapterError::Io)?;
    }
    load_effect_key(path)
}

fn load_effect_key(path: &Path) -> Result<Zeroizing<[u8; 32]>, AdapterError> {
    reject_symlink(path)?;
    let metadata = std::fs::symlink_metadata(path).map_err(AdapterError::Io)?;
    if !metadata.file_type().is_file() || metadata.permissions().mode() & 0o077 != 0 {
        return Err(AdapterError::InvalidResource);
    }
    let bytes = Zeroizing::new(std::fs::read(path).map_err(AdapterError::Io)?);
    let key: [u8; EFFECT_STORE_KEY_BYTES] = bytes
        .as_slice()
        .try_into()
        .map_err(|_| AdapterError::InvalidResource)?;
    Ok(Zeroizing::new(key))
}

pub(super) fn reject_symlink(path: &Path) -> Result<(), AdapterError> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => Err(AdapterError::InvalidResource),
        Ok(_) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(AdapterError::Io(error)),
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

pub(super) fn query_authorized_effect(
    connection: &Connection,
    command_id: &[u8; 16],
) -> Result<Option<AuthorizedEffectRecord>, AdapterError> {
    connection
        .query_row(
            "SELECT command_id,idempotency_key,node_id,operation_id,payload_sha256,action,authorization_revision,effect_kind,resource_key,effect_revision,expires_at,state,delivery_mode,response,record_hmac FROM authorized_effects WHERE command_id=?1",
            [command_id.as_slice()],
            |row| {
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
            },
        )
        .optional()
        .map_err(sqlite_io)
}

pub(super) fn query_artifact(
    connection: &Connection,
    artifact_id: &[u8; 16],
) -> Result<Option<ArtifactRecord>, AdapterError> {
    connection
        .query_row(
            "SELECT artifact_id,certificate_id,certificate_version,operation_id,content_sha256,content_size,artifact_expires_at,state,active_grant_id,active_subject,grant_expires_at,active_offset,record_hmac FROM certificate_artifacts WHERE artifact_id=?1",
            [artifact_id.as_slice()],
            |row| {
                let certificate_version: i64 = row.get(2)?;
                let content_size: i64 = row.get(5)?;
                let active_offset: i64 = row.get(11)?;
                let active_grant: Option<Vec<u8>> = row.get(8)?;
                Ok(ArtifactRecord {
                    artifact_id: fixed_blob(row.get(0)?)?,
                    certificate_id: fixed_blob(row.get(1)?)?,
                    certificate_version: u64::try_from(certificate_version).map_err(|_| rusqlite::Error::InvalidQuery)?,
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
                    active_offset: u64::try_from(active_offset).map_err(|_| rusqlite::Error::InvalidQuery)?,
                    record_hmac: fixed_blob(row.get(12)?)?,
                })
            },
        )
        .optional()
        .map_err(sqlite_io)
}

fn query_certificate_artifacts(
    connection: &Connection,
    certificate_id: &[u8; 16],
) -> Result<Vec<[u8; 16]>, AdapterError> {
    let mut statement = connection
        .prepare("SELECT artifact_id FROM certificate_artifacts WHERE certificate_id=?1")
        .map_err(sqlite_io)?;
    let rows = statement
        .query_map([certificate_id.as_slice()], |row| fixed_blob(row.get(0)?))
        .map_err(sqlite_io)?;
    rows.collect::<Result<Vec<_>, _>>().map_err(sqlite_io)
}

fn query_all_artifacts(connection: &Connection) -> Result<Vec<[u8; 16]>, AdapterError> {
    let mut statement = connection
        .prepare("SELECT artifact_id FROM certificate_artifacts ORDER BY artifact_id")
        .map_err(sqlite_io)?;
    let rows = statement
        .query_map([], |row| fixed_blob(row.get(0)?))
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
        || record.active_offset > record.content_size
        || (record.state == "available" && record.active_offset != 0)
        || authenticate_artifact_record(key, record) != record.record_hmac
    {
        return Err(AdapterError::InvalidResource);
    }
    Ok(())
}

pub(super) fn authenticate_artifact_record(key: &[u8; 32], record: &ArtifactRecord) -> [u8; 32] {
    let mut preimage = Vec::with_capacity(512);
    preimage.extend_from_slice(b"ocservia.certificate-artifact-record.v1\0");
    preimage.extend_from_slice(&record.artifact_id);
    preimage.extend_from_slice(&record.certificate_id);
    preimage.extend_from_slice(&record.certificate_version.to_be_bytes());
    preimage.extend_from_slice(&record.operation_id);
    preimage.extend_from_slice(&record.content_sha256);
    preimage.extend_from_slice(&record.content_size.to_be_bytes());
    preimage.extend_from_slice(&record.artifact_expires_at.to_be_bytes());
    append_effect_field(&mut preimage, record.state.as_bytes());
    match record.active_grant_id {
        Some(grant_id) => {
            preimage.push(1);
            preimage.extend_from_slice(&grant_id);
        }
        None => preimage.push(0),
    }
    append_effect_field(&mut preimage, record.active_subject.as_bytes());
    match record.grant_expires_at {
        Some(expires) => {
            preimage.push(1);
            preimage.extend_from_slice(&expires.to_be_bytes());
        }
        None => preimage.push(0),
    }
    preimage.extend_from_slice(&record.active_offset.to_be_bytes());
    hmac_sha256(key, &[&preimage])
}

fn prune_expired_authorized_effects(
    connection: &Connection,
    key: &[u8; 32],
    now: i64,
) -> Result<(), AdapterError> {
    let command_ids = {
        let mut statement = connection
            .prepare(
                "SELECT command_id FROM authorized_effects WHERE state='applied' AND expires_at<=?1",
            )
            .map_err(sqlite_io)?;
        let rows = statement
            .query_map([now], |row| fixed_blob(row.get(0)?))
            .map_err(sqlite_io)?;
        rows.collect::<Result<Vec<[u8; 16]>, _>>()
            .map_err(sqlite_io)?
    };
    for command_id in command_ids {
        let record =
            query_authorized_effect(connection, &command_id)?.ok_or(AdapterError::Unavailable)?;
        validate_authorized_record(key, &record)?;
        if record.state != "applied" || record.expires_at > now {
            return Err(AdapterError::InvalidResource);
        }
        let changed = connection
            .execute(
                "DELETE FROM authorized_effects WHERE command_id=?1 AND state='applied' AND expires_at<=?2",
                params![command_id.as_slice(), now],
            )
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

pub(super) fn authenticate_authorized_record(
    key: &[u8; 32],
    record: &AuthorizedEffectRecord,
) -> [u8; 32] {
    let mut preimage = Vec::with_capacity(1024 + record.response.len());
    preimage.extend_from_slice(b"ocservia.authorized-effect-record.v1\0");
    preimage.extend_from_slice(&record.command_id);
    preimage.extend_from_slice(&record.idempotency_key);
    preimage.extend_from_slice(&record.node_id);
    preimage.extend_from_slice(&record.operation_id);
    preimage.extend_from_slice(&record.payload_sha256);
    append_effect_field(&mut preimage, record.action.as_bytes());
    preimage.extend_from_slice(&record.authorization_revision.to_be_bytes());
    append_effect_field(&mut preimage, record.effect_kind.as_bytes());
    append_effect_field(&mut preimage, record.resource_key.as_bytes());
    preimage.extend_from_slice(&record.effect_revision.to_be_bytes());
    preimage.extend_from_slice(&record.expires_at.to_be_bytes());
    append_effect_field(&mut preimage, record.state.as_bytes());
    preimage.extend_from_slice(&record.delivery_mode.to_be_bytes());
    append_effect_field(&mut preimage, &record.response);
    hmac_sha256(key, &[&preimage])
}

fn append_effect_field(target: &mut Vec<u8>, value: &[u8]) {
    let length = u64::try_from(value.len()).unwrap_or(u64::MAX);
    target.extend_from_slice(&length.to_be_bytes());
    target.extend_from_slice(value);
}

fn query_effect(
    connection: &Connection,
    mutation_kind: &str,
    resource_key: &str,
) -> Result<Option<EffectRecord>, AdapterError> {
    connection
        .query_row(
            "SELECT mutation_kind,resource_key,revision,command_id,idempotency_key,payload_sha256,expires_at,state,before_hmac,after_hmac,record_hmac FROM desired_effects WHERE mutation_kind=?1 AND resource_key=?2",
            params![mutation_kind, resource_key],
            |row| {
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
            },
        )
        .optional()
        .map_err(sqlite_io)
}

fn query_prepared_effects(
    connection: &Connection,
    config_effects: bool,
) -> Result<Vec<EffectRecord>, AdapterError> {
    let mut statement = connection
        .prepare(
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

fn fixed_blob<const N: usize>(value: Vec<u8>) -> Result<[u8; N], rusqlite::Error> {
    value.try_into().map_err(|_| rusqlite::Error::InvalidQuery)
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
    if expected != record.record_hmac {
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
    let changed = connection
        .execute(
            "UPDATE desired_effects SET state=?4,record_hmac=?5,updated_at=?6 WHERE mutation_kind=?1 AND resource_key=?2 AND revision=?3",
            params![record.mutation_kind, record.resource_key, i64::try_from(record.revision).map_err(|_| AdapterError::InvalidResource)?, state, record_hmac.as_slice(), now],
        )
        .map_err(sqlite_io)?;
    if changed != 1 {
        return Err(AdapterError::Unavailable);
    }
    Ok(())
}

pub(super) fn authenticate_file(key: &[u8; 32], bytes: &[u8]) -> [u8; 32] {
    hmac_sha256(key, &[b"ocservia.authoritative-file.v1\0", bytes])
}

fn authenticate_store_identity(key: &[u8; 32], store_id: &[u8; 16]) -> [u8; 32] {
    hmac_sha256(key, &[b"ocservia.effect-store-identity.v1\0", store_id])
}

#[allow(clippy::too_many_arguments)]
pub(super) fn authenticate_record(
    key: &[u8; 32],
    mutation_kind: &str,
    resource_key: &str,
    revision: u64,
    identity: ValidatedEffectIdentity<'_>,
    state: &str,
    before_hmac: &[u8; 32],
    after_hmac: &[u8; 32],
) -> [u8; 32] {
    hmac_sha256(
        key,
        &[
            b"ocservia.desired-effect-record.v1\0",
            mutation_kind.as_bytes(),
            &[0],
            resource_key.as_bytes(),
            &[0],
            &revision.to_be_bytes(),
            identity.command_id,
            identity.idempotency_key,
            identity.payload_sha256,
            &identity.expires_at.to_be_bytes(),
            state.as_bytes(),
            before_hmac,
            after_hmac,
        ],
    )
}

fn hmac_sha256(key: &[u8; 32], chunks: &[&[u8]]) -> [u8; 32] {
    let mut inner_key = [0x36_u8; 64];
    let mut outer_key = [0x5c_u8; 64];
    for (index, byte) in key.iter().enumerate() {
        inner_key[index] ^= byte;
        outer_key[index] ^= byte;
    }
    let mut inner = Sha256::new();
    inner.update(inner_key);
    for chunk in chunks {
        inner.update(chunk);
    }
    let inner_digest = inner.finalize();
    let mut outer = Sha256::new();
    outer.update(outer_key);
    outer.update(inner_digest);
    outer.finalize().into()
}

fn sqlite_io(error: rusqlite::Error) -> AdapterError {
    AdapterError::Io(io::Error::other(error))
}
