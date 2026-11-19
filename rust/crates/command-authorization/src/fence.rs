use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ConnectionFenceV2, FenceBindingV2, FenceOperationKind, FenceSignatureVersion,
};
use sha2::{Digest as _, Sha256};

use crate::{
    AuthorizationError, CONNECTION_FENCE_V2_DOMAIN_SEPARATOR, FENCE_BINDING_V2_DOMAIN_SEPARATOR,
    MAX_FUTURE_SKEW_SECONDS, append_string, fixed, timestamp_nanos,
};

/// The only signature version of the frozen V2 fence contract: Ed25519 with
/// the sha256 key identifier scheme.
pub const FENCE_SIGNATURE_VERSION_ED25519_V1: u32 = 1;

/// Independent, non-Protobuf canonical claims for one per-node
/// connection-owner fence term.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConnectionFenceClaimsV2 {
    pub signature_version: u32,
    pub key_id: String,
    pub fence_id: [u8; 16],
    pub node_id: [u8; 16],
    pub endpoint_id: [u8; 32],
    pub owner_instance_id: [u8; 16],
    pub owner_incarnation: u64,
    pub owner_epoch: u64,
    pub connection_id: [u8; 16],
    pub authorization_revision: u64,
    pub capabilities: Vec<String>,
    pub lease_until_seconds: i64,
    pub lease_until_nanos: u32,
    pub issued_at_seconds: i64,
    pub issued_at_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

/// Independent, non-Protobuf canonical claims binding one operation identity
/// to the exact fence term of its dispatch attempt.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FenceBindingClaimsV2 {
    pub signature_version: u32,
    pub key_id: String,
    pub operation_kind: u32,
    pub operation_id: [u8; 16],
    pub fence_id: [u8; 16],
    pub node_id: [u8; 16],
    pub endpoint_id: [u8; 32],
    pub owner_instance_id: [u8; 16],
    pub owner_incarnation: u64,
    pub owner_epoch: u64,
    pub connection_id: [u8; 16],
    pub authorization_revision: u64,
    pub capability: String,
    pub issued_at_seconds: i64,
    pub issued_at_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

/// A verified connection-owner fence term. transportd and the Agent use the
/// recorded owner tuple to match operation bindings against the fence of the
/// actual dispatch attempt. The lease deadline keeps its nanosecond part:
/// truncating it to whole seconds would reject leases that are still valid
/// for up to one second.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VerifiedConnectionFenceV2 {
    pub fence_id: [u8; 16],
    pub owner_instance_id: [u8; 16],
    pub owner_incarnation: u64,
    pub owner_epoch: u64,
    pub connection_id: [u8; 16],
    pub authorization_revision: u64,
    pub capabilities: Vec<String>,
    pub lease_until_seconds: i64,
    pub lease_until_nanos: u32,
    pub expires_at_seconds: i64,
    pub expires_at_nanos: u32,
}

impl VerifiedConnectionFenceV2 {
    /// Reports whether the ownership lease deadline has passed at the given
    /// nanosecond-precise instant.
    #[must_use]
    pub fn lease_expired(&self, now_seconds: i64, now_nanos: u32) -> bool {
        (self.lease_until_seconds, self.lease_until_nanos) <= (now_seconds, now_nanos)
    }
}

/// The domain separator of the canonical state-update operation identity.
/// One node-trust update is one operation: the Controller derives the
/// identity from the exact carrier and signs a fence binding over it, while
/// transportd derives it again independently, so a binding for one update
/// can never authorize a different endpoint, state, revision, or reason.
pub const STATE_UPDATE_OPERATION_DOMAIN: &[u8] = b"ocservia/state-update-operation/v1\0";

/// Derives the canonical operation identity of one node trust update: the
/// first 16 bytes of SHA-256 over the domain separator, node id, endpoint
/// id, state code, revision, byte length of the reason, and the reason.
#[must_use]
pub fn state_update_operation_id(
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    state: u32,
    revision: u64,
    reason: &str,
) -> [u8; 16] {
    let mut digest = Sha256::new();
    digest.update(STATE_UPDATE_OPERATION_DOMAIN);
    digest.update(node_id);
    digest.update(endpoint_id);
    digest.update(state.to_be_bytes());
    digest.update(revision.to_be_bytes());
    let reason_bytes = reason.as_bytes();
    digest.update(
        u32::try_from(reason_bytes.len())
            .unwrap_or(u32::MAX)
            .to_be_bytes(),
    );
    digest.update(reason_bytes);
    let digest = digest.finalize();
    let mut operation_id = [0_u8; 16];
    operation_id.copy_from_slice(&digest[..16]);
    operation_id
}

impl FenceBindingClaimsV2 {
    /// Reports whether this binding was signed for exactly the fence term of
    /// the recorded dispatch attempt. A late legitimate result still matches
    /// its own attempt's fence; a result recorded under any other epoch,
    /// connection, or owner does not.
    #[must_use]
    pub fn matches_fence(&self, fence: &VerifiedConnectionFenceV2) -> bool {
        self.fence_id == fence.fence_id
            && self.owner_instance_id == fence.owner_instance_id
            && self.owner_incarnation == fence.owner_incarnation
            && self.owner_epoch == fence.owner_epoch
            && self.connection_id == fence.connection_id
            && self.authorization_revision == fence.authorization_revision
    }
}

/// Projects a Controller-signed `ConnectionFenceV2` into the independent
/// claims model.
///
/// # Errors
///
/// Rejects unsupported signature versions and malformed fixed-width fields.
pub fn connection_fence_claims_v2(
    fence: &ConnectionFenceV2,
) -> Result<ConnectionFenceClaimsV2, AuthorizationError> {
    if signature_version_value(fence.signature_version) != Some(FENCE_SIGNATURE_VERSION_ED25519_V1)
    {
        return Err(AuthorizationError::UnsupportedVersion);
    }
    let lease_until = fence
        .lease_until
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "connection_fence_lease_until_missing",
        ))?;
    let issued_at = fence
        .issued_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "connection_fence_issued_at_missing",
        ))?;
    let expires_at = fence
        .expires_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "connection_fence_expires_at_missing",
        ))?;
    Ok(ConnectionFenceClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: fence.key_id.clone(),
        fence_id: fixed::<16>(&fence.fence_id, "connection_fence_fence_id_invalid")?,
        node_id: fixed::<16>(&fence.node_id, "connection_fence_node_invalid")?,
        endpoint_id: fixed::<32>(&fence.endpoint_id, "connection_fence_endpoint_invalid")?,
        owner_instance_id: fixed::<16>(
            &fence.owner_instance_id,
            "connection_fence_owner_instance_invalid",
        )?,
        owner_incarnation: fence.owner_incarnation,
        owner_epoch: fence.owner_epoch,
        connection_id: fixed::<16>(&fence.connection_id, "connection_fence_connection_invalid")?,
        authorization_revision: fence.authorization_revision,
        capabilities: fence.capabilities.clone(),
        lease_until_seconds: lease_until.seconds,
        lease_until_nanos: timestamp_nanos(lease_until.nanos)?,
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos: timestamp_nanos(issued_at.nanos)?,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos: timestamp_nanos(expires_at.nanos)?,
    })
}

/// Produces the exact bytes signed for `ConnectionFenceV2` without
/// serializing Protobuf. The domain separator is disjoint from every V1
/// contract, so V1 and V2 proofs can never cross-validate.
///
/// # Errors
///
/// Rejects invalid signature versions, key IDs, epochs, revisions,
/// timestamps, lease bounds, and unsorted or duplicate capabilities.
pub fn canonical_connection_fence_v2(
    claims: &ConnectionFenceClaimsV2,
) -> Result<Vec<u8>, AuthorizationError> {
    if claims.signature_version != FENCE_SIGNATURE_VERSION_ED25519_V1
        || claims.key_id.is_empty()
        || claims.key_id.len() > 128
        || claims.owner_epoch == 0
        || claims.authorization_revision == 0
        || claims.capabilities.len() > 128
        || claims.lease_until_nanos >= 1_000_000_000
        || claims.issued_at_nanos >= 1_000_000_000
        || claims.expires_at_nanos >= 1_000_000_000
        || !deadline_strictly_after(
            claims.lease_until_seconds,
            claims.lease_until_nanos,
            claims.issued_at_seconds,
            claims.issued_at_nanos,
        )
        || !deadline_not_after(
            claims.lease_until_seconds,
            claims.lease_until_nanos,
            claims.expires_at_seconds,
            claims.expires_at_nanos,
        )
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "connection_fence_claims_invalid",
        ));
    }
    let mut previous: Option<&str> = None;
    for capability in &claims.capabilities {
        if capability.is_empty()
            || capability.len() > 128
            || previous.is_some_and(|value| value >= capability.as_str())
        {
            return Err(AuthorizationError::ClaimsInvalid(
                "connection_fence_capabilities_invalid",
            ));
        }
        previous = Some(capability);
    }
    let mut encoded = Vec::with_capacity(640);
    encoded.extend_from_slice(CONNECTION_FENCE_V2_DOMAIN_SEPARATOR);
    encoded.extend_from_slice(&claims.signature_version.to_be_bytes());
    append_string(&mut encoded, &claims.key_id)?;
    encoded.extend_from_slice(&claims.fence_id);
    encoded.extend_from_slice(&claims.node_id);
    encoded.extend_from_slice(&claims.endpoint_id);
    encoded.extend_from_slice(&claims.owner_instance_id);
    encoded.extend_from_slice(&claims.owner_incarnation.to_be_bytes());
    encoded.extend_from_slice(&claims.owner_epoch.to_be_bytes());
    encoded.extend_from_slice(&claims.connection_id);
    encoded.extend_from_slice(&claims.authorization_revision.to_be_bytes());
    encoded.extend_from_slice(
        &u32::try_from(claims.capabilities.len())
            .map_err(|_| {
                AuthorizationError::ClaimsInvalid("connection_fence_capabilities_invalid")
            })?
            .to_be_bytes(),
    );
    for capability in &claims.capabilities {
        append_string(&mut encoded, capability)?;
    }
    encoded.extend_from_slice(&claims.lease_until_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.lease_until_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_nanos.to_be_bytes());
    Ok(encoded)
}

/// Projects a Controller-signed `FenceBindingV2` into the independent claims
/// model.
///
/// # Errors
///
/// Rejects unsupported signature versions and malformed fixed-width fields.
pub fn fence_binding_claims_v2(
    binding: &FenceBindingV2,
) -> Result<FenceBindingClaimsV2, AuthorizationError> {
    if signature_version_value(binding.signature_version)
        != Some(FENCE_SIGNATURE_VERSION_ED25519_V1)
    {
        return Err(AuthorizationError::UnsupportedVersion);
    }
    let issued_at = binding
        .issued_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "fence_binding_issued_at_missing",
        ))?;
    let expires_at = binding
        .expires_at
        .as_ref()
        .ok_or(AuthorizationError::ClaimsInvalid(
            "fence_binding_expires_at_missing",
        ))?;
    Ok(FenceBindingClaimsV2 {
        signature_version: FENCE_SIGNATURE_VERSION_ED25519_V1,
        key_id: binding.key_id.clone(),
        operation_kind: fence_operation_kind_value(binding.operation_kind)?,
        operation_id: fixed::<16>(&binding.operation_id, "fence_binding_operation_invalid")?,
        fence_id: fixed::<16>(&binding.fence_id, "fence_binding_fence_id_invalid")?,
        node_id: fixed::<16>(&binding.node_id, "fence_binding_node_invalid")?,
        endpoint_id: fixed::<32>(&binding.endpoint_id, "fence_binding_endpoint_invalid")?,
        owner_instance_id: fixed::<16>(
            &binding.owner_instance_id,
            "fence_binding_owner_instance_invalid",
        )?,
        owner_incarnation: binding.owner_incarnation,
        owner_epoch: binding.owner_epoch,
        connection_id: fixed::<16>(&binding.connection_id, "fence_binding_connection_invalid")?,
        authorization_revision: binding.authorization_revision,
        capability: binding.capability.clone(),
        issued_at_seconds: issued_at.seconds,
        issued_at_nanos: timestamp_nanos(issued_at.nanos)?,
        expires_at_seconds: expires_at.seconds,
        expires_at_nanos: timestamp_nanos(expires_at.nanos)?,
    })
}

/// Produces the exact bytes signed for `FenceBindingV2` without serializing
/// Protobuf.
///
/// # Errors
///
/// Rejects invalid signature versions, key IDs, operation kinds, epochs,
/// revisions, capabilities, and timestamps.
pub fn canonical_fence_binding_v2(
    claims: &FenceBindingClaimsV2,
) -> Result<Vec<u8>, AuthorizationError> {
    if claims.signature_version != FENCE_SIGNATURE_VERSION_ED25519_V1
        || claims.key_id.is_empty()
        || claims.key_id.len() > 128
        || claims.operation_kind == 0
        || claims.operation_kind > 4
        || claims.owner_epoch == 0
        || claims.authorization_revision == 0
        || claims.capability.is_empty()
        || claims.capability.len() > 128
        || claims.issued_at_nanos >= 1_000_000_000
        || claims.expires_at_nanos >= 1_000_000_000
        || claims.expires_at_seconds < claims.issued_at_seconds
        || (claims.expires_at_seconds == claims.issued_at_seconds
            && claims.expires_at_nanos <= claims.issued_at_nanos)
    {
        return Err(AuthorizationError::ClaimsInvalid(
            "fence_binding_claims_invalid",
        ));
    }
    let mut encoded = Vec::with_capacity(512);
    encoded.extend_from_slice(FENCE_BINDING_V2_DOMAIN_SEPARATOR);
    encoded.extend_from_slice(&claims.signature_version.to_be_bytes());
    append_string(&mut encoded, &claims.key_id)?;
    encoded.extend_from_slice(&claims.operation_kind.to_be_bytes());
    encoded.extend_from_slice(&claims.operation_id);
    encoded.extend_from_slice(&claims.fence_id);
    encoded.extend_from_slice(&claims.node_id);
    encoded.extend_from_slice(&claims.endpoint_id);
    encoded.extend_from_slice(&claims.owner_instance_id);
    encoded.extend_from_slice(&claims.owner_incarnation.to_be_bytes());
    encoded.extend_from_slice(&claims.owner_epoch.to_be_bytes());
    encoded.extend_from_slice(&claims.connection_id);
    encoded.extend_from_slice(&claims.authorization_revision.to_be_bytes());
    append_string(&mut encoded, &claims.capability)?;
    encoded.extend_from_slice(&claims.issued_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.issued_at_nanos.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_seconds.to_be_bytes());
    encoded.extend_from_slice(&claims.expires_at_nanos.to_be_bytes());
    Ok(encoded)
}

fn signature_version_value(value: i32) -> Option<u32> {
    match FenceSignatureVersion::try_from(value) {
        Ok(FenceSignatureVersion::Ed25519V1) => Some(FENCE_SIGNATURE_VERSION_ED25519_V1),
        _ => None,
    }
}

fn fence_operation_kind_value(value: i32) -> Result<u32, AuthorizationError> {
    match FenceOperationKind::try_from(value) {
        Ok(FenceOperationKind::Command) => Ok(1),
        Ok(FenceOperationKind::Artifact) => Ok(2),
        Ok(FenceOperationKind::ConnectionClose) => Ok(3),
        Ok(FenceOperationKind::StateUpdate) => Ok(4),
        _ => Err(AuthorizationError::ClaimsInvalid(
            "fence_binding_operation_kind_invalid",
        )),
    }
}

fn deadline_strictly_after(
    until_seconds: i64,
    until_nanos: u32,
    from_seconds: i64,
    from_nanos: u32,
) -> bool {
    until_seconds > from_seconds || (until_seconds == from_seconds && until_nanos > from_nanos)
}

fn deadline_not_after(
    until_seconds: i64,
    until_nanos: u32,
    limit_seconds: i64,
    limit_nanos: u32,
) -> bool {
    until_seconds < limit_seconds || (until_seconds == limit_seconds && until_nanos <= limit_nanos)
}

pub(crate) fn timestamp_expired(
    expires_at_seconds: i64,
    expires_at_nanos: u32,
    now_seconds: i64,
    now_nanos: u32,
) -> bool {
    (expires_at_seconds, expires_at_nanos) <= (now_seconds, now_nanos)
}

pub(crate) fn timestamp_exceeds_future_skew(
    issued_at_seconds: i64,
    issued_at_nanos: u32,
    now_seconds: i64,
    now_nanos: u32,
) -> bool {
    now_seconds
        .checked_add(MAX_FUTURE_SKEW_SECONDS)
        .is_some_and(|limit_seconds| {
            (issued_at_seconds, issued_at_nanos) > (limit_seconds, now_nanos)
        })
}
