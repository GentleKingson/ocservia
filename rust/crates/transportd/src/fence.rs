use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, ConnectionFenceV2, FenceOperationKind,
};
use tonic::Status;
use uuid::Uuid;

/// One signature-verified connection-owner fence term retained per node.
/// `verified` is the authorization truth; `fence` is the verified wire copy
/// returned to Controller readers through `GetOwnerFence`.
#[derive(Clone)]
pub(crate) struct RegisteredFence {
    pub(crate) verified: ocservia_command_authorization::VerifiedConnectionFenceV2,
    pub(crate) fence: ConnectionFenceV2,
    pub(crate) endpoint_id: [u8; 32],
}

#[cfg(test)]
pub(crate) fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(i64::MAX, |elapsed| {
            i64::try_from(elapsed.as_secs()).unwrap_or(i64::MAX)
        })
}

/// Returns the current instant with its nanosecond part. Lease deadlines are
/// compared at this precision: truncating them to whole seconds would reject
/// leases that remain valid for up to one second.
pub(crate) fn unix_now_parts() -> (i64, u32) {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or((i64::MAX, 0), |elapsed| {
            (
                i64::try_from(elapsed.as_secs()).unwrap_or(i64::MAX),
                elapsed.subsec_nanos(),
            )
        })
}

/// Reports whether an ownership lease deadline has passed at the current
/// nanosecond-precise instant.
pub(crate) fn lease_expired_at(lease_until_seconds: i64, lease_until_nanos: u32) -> bool {
    let (now_seconds, now_nanos) = unix_now_parts();
    (lease_until_seconds, lease_until_nanos) <= (now_seconds, now_nanos)
}

/// Returns the positive duration remaining before a lease expires, or `None`
/// if the lease deadline has already elapsed.
pub(crate) fn lease_remaining_duration(
    lease_until_seconds: i64,
    lease_until_nanos: u32,
) -> Option<Duration> {
    let (now_seconds, now_nanos) = unix_now_parts();
    if (lease_until_seconds, lease_until_nanos) <= (now_seconds, now_nanos) {
        return None;
    }
    let delta_secs = lease_until_seconds.checked_sub(now_seconds)?;
    let (secs, nanos) = if lease_until_nanos >= now_nanos {
        (delta_secs, lease_until_nanos - now_nanos)
    } else {
        (
            delta_secs.checked_sub(1)?,
            1_000_000_000 - (now_nanos - lease_until_nanos),
        )
    };
    u64::try_from(secs).ok().map(|s| Duration::new(s, nanos))
}

pub(crate) fn fixed16(value: &[u8]) -> Result<[u8; 16], Status> {
    value
        .try_into()
        .map_err(|_| Status::invalid_argument("identifier must be 16 bytes"))
}

pub(crate) fn fixed32(value: &[u8]) -> Result<[u8; 32], Status> {
    value
        .try_into()
        .map_err(|_| Status::invalid_argument("identifier must be 32 bytes"))
}

/// The registration decision for one incoming fence against the retained
/// term of the same node.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FenceRegistration {
    /// A higher epoch (or the first fence) was recorded.
    Apply,
    /// The same term's lease deadline was refreshed.
    Refresh,
    /// The incoming epoch predates the retained epoch and is rejected.
    Stale,
    /// Two distinct fences claim one epoch, which is never legitimate.
    EpochCollision,
}

pub(crate) fn fence_registration_decision(
    existing: Option<&RegisteredFence>,
    incoming: &RegisteredFence,
) -> FenceRegistration {
    match existing {
        Some(existing) if existing.verified.owner_epoch > incoming.verified.owner_epoch => {
            FenceRegistration::Stale
        }
        Some(existing)
            if existing.verified.owner_epoch == incoming.verified.owner_epoch
                && existing.verified.fence_id != incoming.verified.fence_id =>
        {
            FenceRegistration::EpochCollision
        }
        Some(existing) if existing.verified.fence_id == incoming.verified.fence_id => {
            FenceRegistration::Refresh
        }
        _ => FenceRegistration::Apply,
    }
}

/// Verifies the envelope's fence and binding carriers: both Controller
/// signatures with their node/endpoint pinning, capability membership and
/// exact command capability, carrier identity, and lease validity. The
/// exact-term match against the registered fences is left to the caller so
/// it always runs after every independent check.
pub(crate) fn verify_command_carriers(
    keyring: &ControllerCommandKeyring,
    node_id: &[u8; 16],
    endpoint: &[u8; 32],
    command: &CommandEnvelope,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<
    (
        ocservia_command_authorization::VerifiedConnectionFenceV2,
        ocservia_command_authorization::FenceBindingClaimsV2,
    ),
    Status,
> {
    let (Some(envelope_fence), Some(envelope_binding)) = (
        command.connection_fence.as_ref(),
        command.fence_binding.as_ref(),
    ) else {
        return Err(Status::permission_denied(
            "fenced session commands must carry the owner fence and binding",
        ));
    };
    let verified = keyring
        .verify_connection_fence_v2_at(envelope_fence, node_id, endpoint, now_seconds, now_nanos)
        .map_err(|_| Status::permission_denied("command connection fence is invalid"))?;
    let claims = keyring
        .verify_fence_binding_v2_at(envelope_binding, node_id, endpoint, now_seconds, now_nanos)
        .map_err(|_| Status::permission_denied("command fence binding is invalid"))?;
    if claims.capability != command.required_capability
        || !verified
            .capabilities
            .iter()
            .any(|capability| capability == &claims.capability)
    {
        return Err(Status::permission_denied(
            "fence binding capability does not match the command capability",
        ));
    }
    if claims.operation_kind != FenceOperationKind::Command as u32
        || claims.operation_id.as_slice() != command.command_id.as_slice()
    {
        return Err(Status::permission_denied(
            "fence binding does not match the dispatched command",
        ));
    }
    if lease_expired_at(verified.lease_until_seconds, verified.lease_until_nanos) {
        return Err(Status::failed_precondition(
            "command owner lease has expired",
        ));
    }
    Ok((verified, claims))
}

/// Compares the fence identity tuple only: fence, owner instance,
/// incarnation, epoch, connection, and authorization revision. Lease and
/// expiry fields deliberately differ across lease refreshes of one term.
pub(crate) fn same_fence_term(
    left: &ocservia_command_authorization::VerifiedConnectionFenceV2,
    right: &ocservia_command_authorization::VerifiedConnectionFenceV2,
) -> bool {
    left.fence_id == right.fence_id
        && left.owner_instance_id == right.owner_instance_id
        && left.owner_incarnation == right.owner_incarnation
        && left.owner_epoch == right.owner_epoch
        && left.connection_id == right.connection_id
        && left.authorization_revision == right.authorization_revision
}

pub(crate) fn validate_uuid(value: &[u8], name: &'static str) -> Result<Vec<u8>, Status> {
    let id = Uuid::from_slice(value)
        .map_err(|_| Status::invalid_argument(format!("{name} must be a 16-byte UUID")))?;
    if id.get_version_num() != 7 {
        return Err(Status::invalid_argument(format!("{name} must be UUIDv7")));
    }
    Ok(value.to_vec())
}
