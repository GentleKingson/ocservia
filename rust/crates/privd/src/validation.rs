use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_agent_protocol::{
    ErrorKind, PrivdError, PrivdRequest, PrivilegedRequestMode, privd_request,
};
use ocservia_command_authorization::{
    ArtifactGrantClaimsV1, AuthorizationError, CommandAuthorizationV1, ControllerCommandKeyring,
    FenceBindingClaimsV2, VerifiedConnectionFenceV2,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandDeliveryMode, CommandEnvelope, ConnectionFenceV2, FenceBindingV2, FenceOperationKind,
    SealedSecretPurpose, SealedSecretVersion, SemanticPayloadHashVersion, command_envelope,
};
use uuid::Uuid;

use crate::effect_binding::{effect_binding, error};
use crate::{CONNECTION_FENCING_CAPABILITY, MAX_REQUEST_LIFETIME};

pub(crate) enum ValidatedRequest<'a> {
    Read,
    Execute(
        &'a CommandEnvelope,
        CommandAuthorizationV1,
        prost_types::Timestamp,
        u64,
    ),
    Reconcile(&'a CommandEnvelope, CommandAuthorizationV1),
    ArtifactRead(ArtifactGrantClaimsV1, u64, u64),
    ArtifactConsume(ArtifactGrantClaimsV1, Vec<u8>, u64, bool, u64),
}

#[allow(clippy::too_many_lines)]
pub(crate) fn validate_request<'a>(
    request: &'a PrivdRequest,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
) -> Result<(Duration, ValidatedRequest<'a>), PrivdError> {
    let id = Uuid::from_slice(&request.request_id)
        .map_err(|_| error(ErrorKind::InvalidRequest, "request_id must be UUIDv7"))?;
    if id.get_version_num() != 7 {
        return Err(error(
            ErrorKind::InvalidRequest,
            "request_id must be UUIDv7",
        ));
    }
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
    let now_ms = u64::try_from(now.as_millis())
        .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
    if request.deadline_unix_ms <= now_ms {
        return Err(error(
            ErrorKind::DeadlineExceeded,
            "request deadline exceeded",
        ));
    }
    let remaining = Duration::from_millis(request.deadline_unix_ms - now_ms);
    if remaining > MAX_REQUEST_LIFETIME {
        return Err(error(
            ErrorKind::InvalidRequest,
            "request deadline too far in future",
        ));
    }
    let mode = PrivilegedRequestMode::try_from(request.privileged_mode)
        .unwrap_or(PrivilegedRequestMode::Unspecified);
    let validated = if let Some(command) = request.authorization_command.as_ref() {
        if request.operation.is_some() || mode == PrivilegedRequestMode::Unspecified {
            return Err(error(
                ErrorKind::InvalidRequest,
                "signed command request shape invalid",
            ));
        }
        let now_seconds = i64::try_from(now.as_secs())
            .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
        let claims = command_keys
            .verify_command(command, node_id, now_seconds)
            .map_err(|failure| authorization_error(&failure))?;
        validate_privileged_payload(command, &claims)?;
        let delivery = CommandDeliveryMode::try_from(command.delivery_mode)
            .unwrap_or(CommandDeliveryMode::Unspecified);
        match (mode, delivery) {
            (
                PrivilegedRequestMode::Execute,
                CommandDeliveryMode::ExecuteOrReplay | CommandDeliveryMode::RetryIfEffectAbsent,
            ) => {
                let accepted = request.accepted_at.ok_or_else(|| {
                    error(
                        ErrorKind::InvalidRequest,
                        "journal acceptance timestamp required",
                    )
                })?;
                if accepted.seconds < claims.issued_at_seconds
                    || accepted.seconds > now_seconds.saturating_add(300)
                    || !(0..1_000_000_000).contains(&accepted.nanos)
                {
                    return Err(error(
                        ErrorKind::InvalidRequest,
                        "journal acceptance timestamp invalid",
                    ));
                }
                let owner_epoch = verify_execution_fence(
                    command,
                    &claims,
                    node_id,
                    endpoint_id,
                    command_keys,
                    now_seconds,
                    now.subsec_nanos(),
                )?;
                ValidatedRequest::Execute(command, claims, accepted, owner_epoch)
            }
            (PrivilegedRequestMode::Reconcile, CommandDeliveryMode::ReconcileOnly) => {
                if effect_binding(command, &claims).is_none() {
                    return Err(error(
                        ErrorKind::InvalidRequest,
                        "command does not support privileged reconciliation",
                    ));
                }
                ValidatedRequest::Reconcile(command, claims)
            }
            _ => {
                return Err(error(
                    ErrorKind::PermissionDenied,
                    "signed command delivery mode mismatch",
                ));
            }
        }
    } else {
        if request.accepted_at.is_some() {
            return Err(error(
                ErrorKind::InvalidRequest,
                "journal acceptance timestamp is only valid for execution",
            ));
        }
        if mode != PrivilegedRequestMode::Unspecified {
            return Err(error(
                ErrorKind::PermissionDenied,
                "Controller command authorization required",
            ));
        }
        if let Some(privd_request::Operation::ArtifactRead(value)) = request.operation.as_ref() {
            let grant = value
                .grant
                .as_ref()
                .ok_or_else(|| error(ErrorKind::PermissionDenied, "artifact grant required"))?;
            let artifact_id: [u8; 16] = grant
                .artifact_id
                .as_slice()
                .try_into()
                .map_err(|_| error(ErrorKind::InvalidRequest, "artifact ID invalid"))?;
            let now_seconds = i64::try_from(now.as_secs())
                .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
            let claims = command_keys
                .verify_artifact_grant(
                    grant,
                    node_id,
                    &artifact_id,
                    "certificate_p12",
                    grant.max_bytes,
                    now_seconds,
                )
                .map_err(|failure| authorization_error(&failure))?;
            validate_artifact_claims(&claims)?;
            let owner_epoch = verify_artifact_fence(
                value.connection_fence.as_deref(),
                value.fence_binding.as_deref(),
                &claims.artifact_id,
                node_id,
                endpoint_id,
                command_keys,
                now_seconds,
                now.subsec_nanos(),
            )?;
            if value.offset > claims.max_bytes {
                return Err(error(ErrorKind::InvalidRequest, "artifact offset invalid"));
            }
            ValidatedRequest::ArtifactRead(claims, value.offset, owner_epoch)
        } else if let Some(privd_request::Operation::ArtifactConsume(value)) =
            request.operation.as_ref()
        {
            let grant = value
                .grant
                .as_ref()
                .ok_or_else(|| error(ErrorKind::PermissionDenied, "artifact grant required"))?;
            let artifact_id: [u8; 16] = grant
                .artifact_id
                .as_slice()
                .try_into()
                .map_err(|_| error(ErrorKind::InvalidRequest, "artifact ID invalid"))?;
            let now_seconds = i64::try_from(now.as_secs())
                .map_err(|_| error(ErrorKind::Unavailable, "system clock unavailable"))?;
            let claims = if value.confirm_only {
                command_keys.verify_artifact_grant_for_confirmation(
                    grant,
                    node_id,
                    &artifact_id,
                    "certificate_p12",
                    grant.max_bytes,
                    now_seconds,
                )
            } else {
                command_keys.verify_artifact_grant(
                    grant,
                    node_id,
                    &artifact_id,
                    "certificate_p12",
                    grant.max_bytes,
                    now_seconds,
                )
            }
            .map_err(|failure| authorization_error(&failure))?;
            validate_artifact_claims(&claims)?;
            let owner_epoch = verify_artifact_fence(
                value.connection_fence.as_deref(),
                value.fence_binding.as_deref(),
                &claims.grant_id,
                node_id,
                endpoint_id,
                command_keys,
                now_seconds,
                now.subsec_nanos(),
            )?;
            if value.sha256.len() != 32 || value.size == 0 || value.size > claims.max_bytes {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "artifact consumption evidence invalid",
                ));
            }
            ValidatedRequest::ArtifactConsume(
                claims,
                value.sha256.clone(),
                value.size,
                value.confirm_only,
                owner_epoch,
            )
        } else if !matches!(
            request.operation,
            Some(
                privd_request::Operation::ServiceStatus(_)
                    | privd_request::Operation::OcservVersion(_)
                    | privd_request::Operation::SessionList(_)
                    | privd_request::Operation::IpBanList(_)
                    | privd_request::Operation::ConfigFingerprint(_)
                    | privd_request::Operation::UserList(_)
                    | privd_request::Operation::GroupList(_)
                    | privd_request::Operation::UpgradeResultList(_)
            )
        ) {
            return Err(error(
                ErrorKind::PermissionDenied,
                "Controller command authorization required",
            ));
        } else {
            ValidatedRequest::Read
        }
    };
    Ok((remaining, validated))
}

fn validate_artifact_claims(claims: &ArtifactGrantClaimsV1) -> Result<(), PrivdError> {
    if [
        claims.node_id,
        claims.artifact_id,
        claims.certificate_id,
        claims.operation_id,
        claims.grant_id,
    ]
    .into_iter()
    .any(|value| Uuid::from_bytes(value).get_version_num() != 7)
        || claims.certificate_version == 0
        || claims.max_bytes == 0
        || claims.max_bytes > 64 * 1024 * 1024
        || claims.authorized_subject.is_empty()
        || claims.authorized_subject.len() > 256
    {
        return Err(error(
            ErrorKind::InvalidRequest,
            "artifact grant claims invalid",
        ));
    }
    Ok(())
}

fn authorization_error(failure: &AuthorizationError) -> PrivdError {
    error(ErrorKind::PermissionDenied, failure.code())
}

/// The Agent's own owner-term checks do not survive an Agent compromise, so
/// root re-verifies the Controller-signed connection fence and its command
/// binding at its own clock before any new effect.
fn verify_execution_fence(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<u64, PrivdError> {
    let (fence, binding) = verify_fence_carriers(
        command.connection_fence.as_ref(),
        command.fence_binding.as_ref(),
        node_id,
        endpoint_id,
        command_keys,
        now_seconds,
        now_nanos,
    )?;
    if binding.operation_kind != FenceOperationKind::Command as u32
        || binding.operation_id != claims.command_id
        || binding.capability != claims.required_capability
        || claims.expected_revision != fence.authorization_revision
        || !fence.capabilities.contains(&binding.capability)
        || !binding.matches_fence(&fence)
    {
        return Err(error(
            ErrorKind::PermissionDenied,
            "connection fence does not authorize this command",
        ));
    }
    Ok(fence.owner_epoch)
}

/// Verifies the signed owner term and binding carriers against privd's own
/// clock, pinned node ID and root-provisioned `EndpointID`.
fn verify_fence_carriers(
    fence: Option<&ConnectionFenceV2>,
    binding: Option<&FenceBindingV2>,
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<(VerifiedConnectionFenceV2, FenceBindingClaimsV2), PrivdError> {
    let (Some(fence), Some(binding)) = (fence, binding) else {
        return Err(error(
            ErrorKind::PermissionDenied,
            "signed connection fence and binding required",
        ));
    };
    let fence = command_keys
        .verify_connection_fence_v2_at(fence, node_id, endpoint_id, now_seconds, now_nanos)
        .map_err(|failure| authorization_error(&failure))?;
    if fence.lease_expired(now_seconds, now_nanos) {
        return Err(error(
            ErrorKind::PermissionDenied,
            "connection-owner lease expired",
        ));
    }
    let binding = command_keys
        .verify_fence_binding_v2_at(binding, node_id, endpoint_id, now_seconds, now_nanos)
        .map_err(|failure| authorization_error(&failure))?;
    Ok((fence, binding))
}

/// Artifact reads and consumption are not commands, so the Agent's owner-term
/// check is re-proven here from the same Controller-signed artifact binding the
/// Agent verified: fetch binds the artifact ID, consumption binds the grant ID.
/// Each read re-checks the one dispatch fence, which suffices only while a P12
/// fits one 256 KiB chunk (the 256 KiB chain cap with an RSA-4096 key yields
/// ~192 KiB). If artifacts can span chunks, the Controller must refresh the
/// fence during the transfer.
#[allow(clippy::too_many_arguments)]
fn verify_artifact_fence(
    fence: Option<&ConnectionFenceV2>,
    binding: Option<&FenceBindingV2>,
    operation_id: &[u8; 16],
    node_id: &[u8; 16],
    endpoint_id: &[u8; 32],
    command_keys: &ControllerCommandKeyring,
    now_seconds: i64,
    now_nanos: u32,
) -> Result<u64, PrivdError> {
    let (fence, binding) = verify_fence_carriers(
        fence,
        binding,
        node_id,
        endpoint_id,
        command_keys,
        now_seconds,
        now_nanos,
    )?;
    if binding.operation_kind != FenceOperationKind::Artifact as u32
        || binding.operation_id.as_slice() != operation_id
        || binding.capability != CONNECTION_FENCING_CAPABILITY
        || !fence.capabilities.contains(&binding.capability)
        || !binding.matches_fence(&fence)
    {
        return Err(error(
            ErrorKind::PermissionDenied,
            "connection fence does not authorize this artifact operation",
        ));
    }
    Ok(fence.owner_epoch)
}

#[allow(clippy::too_many_lines)]
fn validate_privileged_payload(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
) -> Result<(), PrivdError> {
    for value in [
        claims.command_id,
        claims.idempotency_key,
        claims.node_id,
        claims.operation_id,
    ] {
        if Uuid::from_bytes(value).get_version_num() != 7 {
            return Err(error(
                ErrorKind::InvalidRequest,
                "signed command UUID identity invalid",
            ));
        }
    }
    match command.payload.as_ref() {
        Some(
            command_envelope::Payload::CompleteConfigPlan(_)
            | command_envelope::Payload::CompleteConfigApply(_),
        ) => {
            // `verify_command` already verified the V2 canonical semantic hash
            // and `candidate_hash` binding for complete config commands.
            if claims.semantic_hash_version != SemanticPayloadHashVersion::V2 as u32 {
                return Err(error(
                    ErrorKind::PermissionDenied,
                    "complete_config_identity_invalid",
                ));
            }
        }
        Some(command_envelope::Payload::SessionDisconnect(payload)) => {
            validate_session(&payload.session_id, &payload.boot_id)?;
        }
        Some(command_envelope::Payload::SessionTerminate(payload)) => {
            validate_session(&payload.session_id, &payload.boot_id)?;
        }
        Some(command_envelope::Payload::IpBanRemove(payload)) => {
            let canonical = payload
                .ip
                .parse::<std::net::IpAddr>()
                .map_err(|_| error(ErrorKind::InvalidRequest, "IP address invalid"))?
                .to_string();
            if canonical != payload.ip {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "IP address must be canonical",
                ));
            }
        }
        Some(command_envelope::Payload::GroupApply(payload)) => {
            if payload.members.len() > ocservia_agent_protocol::MAX_MANAGED_RESOURCES
                || payload.members.windows(2).any(|pair| pair[0] >= pair[1])
            {
                return Err(error(ErrorKind::InvalidRequest, "group members invalid"));
            }
        }
        Some(command_envelope::Payload::CertificateCsr(payload)) => {
            validate_uuid(&payload.certificate_id, "certificate ID invalid")?;
        }
        Some(command_envelope::Payload::CertificateRevoke(payload)) => {
            validate_uuid(&payload.certificate_id, "certificate ID invalid")?;
            if payload.certificate_version == 0 {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "certificate version invalid",
                ));
            }
        }
        Some(command_envelope::Payload::CertificateP12(payload)) => {
            validate_uuid(&payload.certificate_id, "certificate ID invalid")?;
            validate_uuid(&payload.artifact_id, "artifact ID invalid")?;
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::CertificateP12Password,
            )?;
            if !payload.sealed_password.is_empty()
                || !payload.secret_key_id.is_empty()
                || payload.certificate_version == 0
                || payload.artifact_expires_at.is_none()
            {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "P12 secret binding invalid",
                ));
            }
        }
        Some(command_envelope::Payload::UserCreate(payload)) => {
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::UserPassword,
            )?;
            if !payload.sealed_password.is_empty() || !payload.secret_key_id.is_empty() {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "user secret binding invalid",
                ));
            }
        }
        Some(command_envelope::Payload::UserPasswordRotate(payload)) => {
            validate_sealed_secret(
                payload.sealed_password_v1.as_ref(),
                SealedSecretPurpose::UserPassword,
            )?;
            if !payload.sealed_password.is_empty() || !payload.secret_key_id.is_empty() {
                return Err(error(
                    ErrorKind::InvalidRequest,
                    "user secret binding invalid",
                ));
            }
        }
        Some(
            command_envelope::Payload::ServiceReload(_)
            | command_envelope::Payload::UserDisable(_)
            | command_envelope::Payload::UserEnable(_),
        ) => {}
        Some(command_envelope::Payload::AgentUpgrade(payload)) => {
            validate_upgrade_release(payload)?;
        }
        _ => {
            return Err(error(
                ErrorKind::PermissionDenied,
                "payload is not a privileged privd command",
            ));
        }
    }
    Ok(())
}

fn validate_sealed_secret(
    secret: Option<&ocservia_contracts::generated::ocserv::platform::agent::v1::SealedSecretV1>,
    purpose: SealedSecretPurpose,
) -> Result<(), PrivdError> {
    let secret =
        secret.ok_or_else(|| error(ErrorKind::InvalidRequest, "sealed secret required"))?;
    if SealedSecretVersion::try_from(secret.version).ok() != Some(SealedSecretVersion::V1)
        || SealedSecretPurpose::try_from(secret.purpose).ok() != Some(purpose)
        || secret.key_id.is_empty()
        || secret.key_id.len() > 128
        || secret.ciphertext.len() < 32
        || secret.ciphertext.len() > 16 * 1024
    {
        return Err(error(ErrorKind::InvalidRequest, "sealed secret invalid"));
    }
    Ok(())
}

/// Independently validates the immutable upgrade release identity carried by a
/// Controller-signed command. Privd never trusts the Agent-side validation and
/// never executes package installation at this boundary.
fn validate_upgrade_release(
    payload: &ocservia_contracts::generated::ocserv::platform::agent::v1::AgentUpgrade,
) -> Result<(), PrivdError> {
    if !ocservia_contracts::agent_upgrade::valid_target_version(&payload.target_version) {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade target version invalid",
        ));
    }
    if payload.package_sha256.len() != 32 {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade package digest invalid",
        ));
    }
    if !ocservia_contracts::agent_upgrade::valid_architecture(&payload.architecture) {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade architecture invalid",
        ));
    }
    if ocservia_contracts::agent_upgrade::runtime_architecture()
        != Some(payload.architecture.as_str())
    {
        return Err(error(
            ErrorKind::InvalidRequest,
            "agent upgrade architecture does not match this host",
        ));
    }
    Ok(())
}

fn validate_session(session_id: &str, boot_id: &str) -> Result<(), PrivdError> {
    let parsed = session_id
        .parse::<u64>()
        .map_err(|_| error(ErrorKind::InvalidRequest, "session ID invalid"))?;
    if parsed == 0 || parsed.to_string() != session_id || Uuid::parse_str(boot_id).is_err() {
        return Err(error(ErrorKind::InvalidRequest, "session target invalid"));
    }
    Ok(())
}

fn validate_uuid(value: &[u8], detail: &str) -> Result<(), PrivdError> {
    let id = Uuid::from_slice(value).map_err(|_| error(ErrorKind::InvalidRequest, detail))?;
    if id.get_version_num() != 7 {
        return Err(error(ErrorKind::InvalidRequest, detail));
    }
    Ok(())
}
