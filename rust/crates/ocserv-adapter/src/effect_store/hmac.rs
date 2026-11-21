use super::super::{Digest, Sha256};
use super::artifacts::ArtifactRecord;
use super::authorized::AuthorizedEffectRecord;
use super::desired::ValidatedEffectIdentity;
use super::owner_fence::OwnerFenceRecord;

struct HmacSha256 {
    inner: Sha256,
    outer_key: [u8; 64],
}

impl HmacSha256 {
    fn new(key: &[u8; 32]) -> Self {
        let mut inner_key = [0x36_u8; 64];
        let mut outer_key = [0x5c_u8; 64];
        for (index, byte) in key.iter().enumerate() {
            inner_key[index] ^= byte;
            outer_key[index] ^= byte;
        }
        let mut inner = Sha256::new();
        inner.update(inner_key);
        Self { inner, outer_key }
    }

    fn update(&mut self, bytes: &[u8]) {
        self.inner.update(bytes);
    }

    fn update_field(&mut self, value: &[u8]) {
        let length = u64::try_from(value.len()).unwrap_or(u64::MAX);
        self.inner.update(length.to_be_bytes());
        self.inner.update(value);
    }

    fn finalize(self) -> [u8; 32] {
        let inner_digest = self.inner.finalize();
        let mut outer = Sha256::new();
        outer.update(self.outer_key);
        outer.update(inner_digest);
        outer.finalize().into()
    }
}

pub(crate) fn authenticate_file(key: &[u8; 32], bytes: &[u8]) -> [u8; 32] {
    let mut mac = HmacSha256::new(key);
    mac.update(b"ocservia.authoritative-file.v1\0");
    mac.update(bytes);
    mac.finalize()
}

pub(crate) fn authenticate_store_identity(key: &[u8; 32], store_id: &[u8; 16]) -> [u8; 32] {
    let mut mac = HmacSha256::new(key);
    mac.update(b"ocservia.effect-store-identity.v1\0");
    mac.update(store_id);
    mac.finalize()
}

pub(crate) fn authenticate_owner_fence_record(
    key: &[u8; 32],
    record: &OwnerFenceRecord,
) -> [u8; 32] {
    let mut mac = HmacSha256::new(key);
    mac.update(b"ocservia.owner-epoch-floor.v1\0");
    mac.update(&record.node_id);
    mac.update(&record.endpoint_id);
    mac.update(&record.owner_epoch.to_be_bytes());
    mac.finalize()
}

#[allow(clippy::too_many_arguments)]
pub(crate) fn authenticate_record(
    key: &[u8; 32],
    mutation_kind: &str,
    resource_key: &str,
    revision: u64,
    identity: ValidatedEffectIdentity<'_>,
    state: &str,
    before_hmac: &[u8; 32],
    after_hmac: &[u8; 32],
) -> [u8; 32] {
    let mut mac = HmacSha256::new(key);
    mac.update(b"ocservia.desired-effect-record.v1\0");
    mac.update(mutation_kind.as_bytes());
    mac.update(&[0]);
    mac.update(resource_key.as_bytes());
    mac.update(&[0]);
    mac.update(&revision.to_be_bytes());
    mac.update(identity.command_id);
    mac.update(identity.idempotency_key);
    mac.update(identity.payload_sha256);
    mac.update(&identity.expires_at.to_be_bytes());
    mac.update(state.as_bytes());
    mac.update(before_hmac);
    mac.update(after_hmac);
    mac.finalize()
}

pub(crate) fn authenticate_authorized_record(
    key: &[u8; 32],
    record: &AuthorizedEffectRecord,
) -> [u8; 32] {
    let mut mac = HmacSha256::new(key);
    mac.update(b"ocservia.authorized-effect-record.v1\0");
    mac.update(&record.command_id);
    mac.update(&record.idempotency_key);
    mac.update(&record.node_id);
    mac.update(&record.operation_id);
    mac.update(&record.payload_sha256);
    mac.update_field(record.action.as_bytes());
    mac.update(&record.authorization_revision.to_be_bytes());
    mac.update_field(record.effect_kind.as_bytes());
    mac.update_field(record.resource_key.as_bytes());
    mac.update(&record.effect_revision.to_be_bytes());
    mac.update(&record.expires_at.to_be_bytes());
    mac.update_field(record.state.as_bytes());
    mac.update(&record.delivery_mode.to_be_bytes());
    mac.update_field(&record.response);
    mac.finalize()
}

pub(crate) fn authenticate_artifact_record(key: &[u8; 32], record: &ArtifactRecord) -> [u8; 32] {
    let mut mac = HmacSha256::new(key);
    mac.update(b"ocservia.certificate-artifact-record.v1\0");
    mac.update(&record.artifact_id);
    mac.update(&record.certificate_id);
    mac.update(&record.certificate_version.to_be_bytes());
    mac.update(&record.operation_id);
    mac.update(&record.content_sha256);
    mac.update(&record.content_size.to_be_bytes());
    mac.update(&record.artifact_expires_at.to_be_bytes());
    mac.update_field(record.state.as_bytes());
    match record.active_grant_id {
        Some(grant_id) => {
            mac.update(&[1]);
            mac.update(&grant_id);
        }
        None => mac.update(&[0]),
    }
    mac.update_field(record.active_subject.as_bytes());
    match record.grant_expires_at {
        Some(expires) => {
            mac.update(&[1]);
            mac.update(&expires.to_be_bytes());
        }
        None => mac.update(&[0]),
    }
    mac.update(&record.active_offset.to_be_bytes());
    mac.finalize()
}
