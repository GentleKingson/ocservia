use hmac::{Hmac, Mac};

use super::super::Sha256;
use super::artifacts::ArtifactRecord;
use super::authorized::AuthorizedEffectRecord;
use super::desired::ValidatedEffectIdentity;
use super::owner_fence::OwnerFenceRecord;

type HmacSha256 = Hmac<Sha256>;

fn new_mac(key: &[u8; 32]) -> HmacSha256 {
    HmacSha256::new_from_slice(key).expect("HMAC-SHA256 accepts any 32-byte key")
}

fn update_field(mac: &mut HmacSha256, value: &[u8]) {
    let length = u64::try_from(value.len()).unwrap_or(u64::MAX);
    mac.update(&length.to_be_bytes());
    mac.update(value);
}

fn finalize_mac(mac: HmacSha256) -> [u8; 32] {
    mac.finalize().into_bytes().into()
}

pub(crate) fn verify_mac(expected: &[u8; 32], actual: &[u8; 32]) -> bool {
    let mut diff = 0_u8;
    for (lhs, rhs) in expected.iter().zip(actual.iter()) {
        diff |= lhs ^ rhs;
    }
    diff == 0
}

pub(crate) fn authenticate_file(key: &[u8; 32], bytes: &[u8]) -> [u8; 32] {
    let mut mac = new_mac(key);
    mac.update(b"ocservia.authoritative-file.v1\0");
    mac.update(bytes);
    finalize_mac(mac)
}

pub(crate) fn authenticate_store_identity(key: &[u8; 32], store_id: &[u8; 16]) -> [u8; 32] {
    let mut mac = new_mac(key);
    mac.update(b"ocservia.effect-store-identity.v1\0");
    mac.update(store_id);
    finalize_mac(mac)
}

pub(crate) fn authenticate_owner_fence_record(
    key: &[u8; 32],
    record: &OwnerFenceRecord,
) -> [u8; 32] {
    let mut mac = new_mac(key);
    mac.update(b"ocservia.owner-epoch-floor.v1\0");
    mac.update(&record.node_id);
    mac.update(&record.endpoint_id);
    mac.update(&record.owner_epoch.to_be_bytes());
    finalize_mac(mac)
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
    let mut mac = new_mac(key);
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
    finalize_mac(mac)
}

pub(crate) fn authenticate_authorized_record(
    key: &[u8; 32],
    record: &AuthorizedEffectRecord,
) -> [u8; 32] {
    let mut mac = new_mac(key);
    mac.update(b"ocservia.authorized-effect-record.v1\0");
    mac.update(&record.command_id);
    mac.update(&record.idempotency_key);
    mac.update(&record.node_id);
    mac.update(&record.operation_id);
    mac.update(&record.payload_sha256);
    update_field(&mut mac, record.action.as_bytes());
    mac.update(&record.authorization_revision.to_be_bytes());
    update_field(&mut mac, record.effect_kind.as_bytes());
    update_field(&mut mac, record.resource_key.as_bytes());
    mac.update(&record.effect_revision.to_be_bytes());
    mac.update(&record.expires_at.to_be_bytes());
    update_field(&mut mac, record.state.as_bytes());
    mac.update(&record.delivery_mode.to_be_bytes());
    update_field(&mut mac, &record.response);
    finalize_mac(mac)
}

pub(crate) fn authenticate_artifact_record(key: &[u8; 32], record: &ArtifactRecord) -> [u8; 32] {
    let mut mac = new_mac(key);
    mac.update(b"ocservia.certificate-artifact-record.v1\0");
    mac.update(&record.artifact_id);
    mac.update(&record.certificate_id);
    mac.update(&record.certificate_version.to_be_bytes());
    mac.update(&record.operation_id);
    mac.update(&record.content_sha256);
    mac.update(&record.content_size.to_be_bytes());
    mac.update(&record.artifact_expires_at.to_be_bytes());
    update_field(&mut mac, record.state.as_bytes());
    match record.active_grant_id {
        Some(grant_id) => {
            mac.update(&[1]);
            mac.update(&grant_id);
        }
        None => mac.update(&[0]),
    }
    update_field(&mut mac, record.active_subject.as_bytes());
    match record.grant_expires_at {
        Some(expires) => {
            mac.update(&[1]);
            mac.update(&expires.to_be_bytes());
        }
        None => mac.update(&[0]),
    }
    finalize_mac(mac)
}
