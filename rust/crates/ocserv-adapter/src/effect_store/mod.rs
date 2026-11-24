mod artifacts;
mod authorized;
mod desired;
mod hmac;
mod owner_fence;

use super::{
    AdapterError, Connection, EFFECT_STORE_KEY_BYTES, FixedResources, OpenFlags, OpenOptionsExt,
    Path, PermissionsExt, Rng, StdOpenOptions, SystemTime, UNIX_EPOCH, Uuid, Zeroizing, io, params,
};

#[cfg(test)]
pub(super) use self::artifacts::query_artifact;
#[cfg(test)]
pub(super) use self::authorized::query_authorized_effect;
#[cfg(test)]
pub(super) use self::desired::validate_effect_context;
pub(super) use self::desired::{PrepareEffect, validate_effect_identity};
#[cfg(test)]
pub(super) use self::hmac::{
    authenticate_artifact_record, authenticate_authorized_record, authenticate_file,
    authenticate_record,
};

use self::hmac::{authenticate_store_identity, verify_mac};

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
                "PRAGMA busy_timeout=5000;
                 PRAGMA journal_mode=DELETE;
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
                 CREATE INDEX desired_effects_state_idx
                   ON desired_effects(state);",
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
        store.ensure_owner_fence_schema()?;
        store.ensure_receipt_sequence_schema()?;
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
        if !schema.contains("'absent'") {
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
                .map_err(sqlite_io)?;
        }
        self.connection
            .execute_batch(
                "CREATE INDEX IF NOT EXISTS desired_effects_state_idx
                   ON desired_effects(state);",
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
                 ) STRICT;
                 CREATE INDEX IF NOT EXISTS authorized_effects_state_expires_idx
                   ON authorized_effects(state, expires_at);",
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
                   content_size INTEGER NOT NULL CHECK(content_size BETWEEN 0 AND 262144),
                   artifact_expires_at INTEGER NOT NULL,
                   state TEXT NOT NULL CHECK(state IN ('prepared','available','leased','consumed','expired','revoked')),
                   active_grant_id BLOB CHECK(active_grant_id IS NULL OR length(active_grant_id)=16),
                   active_subject TEXT NOT NULL CHECK(length(active_subject) <= 256),
                   grant_expires_at INTEGER,
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

    fn ensure_owner_fence_schema(&self) -> Result<(), AdapterError> {
        self.connection
            .execute_batch(
                "CREATE TABLE IF NOT EXISTS owner_epoch_floors (
                   node_id BLOB NOT NULL CHECK(length(node_id)=16),
                   endpoint_id BLOB NOT NULL CHECK(length(endpoint_id)=32),
                   owner_epoch INTEGER NOT NULL CHECK(owner_epoch > 0),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL,
                   PRIMARY KEY(node_id, endpoint_id)
                 ) STRICT;",
            )
            .map_err(sqlite_io)
    }

    fn ensure_receipt_sequence_schema(&self) -> Result<(), AdapterError> {
        self.connection
            .execute_batch(
                "CREATE TABLE IF NOT EXISTS receipt_sequence (
                   singleton INTEGER PRIMARY KEY CHECK(singleton=1),
                   last_sequence INTEGER NOT NULL CHECK(last_sequence > 0),
                   record_hmac BLOB NOT NULL CHECK(length(record_hmac)=32),
                   updated_at INTEGER NOT NULL
                 ) STRICT;",
            )
            .map_err(sqlite_io)
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
        if !verify_mac(
            &authenticate_store_identity(&self.hmac_key, &store_id),
            &identity_hmac,
        ) {
            return Err(AdapterError::InvalidResource);
        }
        Ok(())
    }
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

fn fixed_blob<const N: usize>(value: Vec<u8>) -> Result<[u8; N], rusqlite::Error> {
    value.try_into().map_err(|_| rusqlite::Error::InvalidQuery)
}

fn sqlite_io(error: rusqlite::Error) -> AdapterError {
    AdapterError::Io(io::Error::other(error))
}
