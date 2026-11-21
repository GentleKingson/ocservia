use ocservia_agent_protocol::{ErrorKind, PrivdError, PrivdResponse, privd_response};
use ocservia_command_authorization::{ArtifactGrantClaimsV1, CommandAuthorizationV1};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandDeliveryMode, CommandEnvelope, command_envelope,
};
use ocservia_ocserv_adapter::{
    Adapter, ArtifactLeaseIdentity, AuthorizedEffectIdentity, EffectIdentity, OwnerFenceIdentity,
};
use prost::Message as _;
use uuid::Uuid;

pub(crate) struct CommandEffectBinding {
    pub(crate) effect_kind: &'static str,
    pub(crate) resource_key: String,
    pub(crate) effect_revision: u64,
}

#[allow(clippy::too_many_lines)]
pub(crate) fn effect_binding(
    command: &CommandEnvelope,
    claims: &CommandAuthorizationV1,
) -> Option<CommandEffectBinding> {
    let (effect_kind, resource_key, effect_revision) = match command.payload.as_ref()? {
        command_envelope::Payload::SessionDisconnect(payload) => (
            "session_disconnect",
            format!("{}:{}", payload.boot_id, payload.session_id),
            claims.expected_revision,
        ),
        command_envelope::Payload::SessionTerminate(payload) => (
            "session_terminate",
            format!("{}:{}", payload.boot_id, payload.session_id),
            claims.expected_revision,
        ),
        command_envelope::Payload::IpBanRemove(payload) => (
            "ip_ban_remove",
            payload.ip.clone(),
            claims.expected_revision,
        ),
        command_envelope::Payload::ServiceReload(_) => (
            "service_reload",
            "ocserv.service".to_owned(),
            claims.expected_revision,
        ),
        command_envelope::Payload::UserCreate(payload) => (
            "user_create",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserDisable(payload) => (
            "user_disable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserEnable(payload) => (
            "user_enable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserPasswordRotate(payload) => (
            "user_password_rotate",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::GroupApply(payload) => (
            "group_apply",
            payload.group_name.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::CompleteConfigPlan(payload) => (
            "config_plan",
            hex::encode(&payload.candidate_hash),
            claims.expected_revision,
        ),
        command_envelope::Payload::CompleteConfigApply(payload) => (
            "config_apply",
            "ocserv.conf".to_owned(),
            payload.desired_revision,
        ),
        command_envelope::Payload::CertificateCsr(payload) => (
            "certificate_csr",
            Uuid::from_slice(&payload.certificate_id).ok()?.to_string(),
            claims.expected_revision,
        ),
        command_envelope::Payload::CertificateRevoke(payload) => (
            "certificate_revoke",
            Uuid::from_slice(&payload.certificate_id).ok()?.to_string(),
            claims.expected_revision,
        ),
        command_envelope::Payload::CertificateP12(payload) => (
            "certificate_p12",
            format!(
                "{}:{}",
                Uuid::from_slice(&payload.certificate_id).ok()?,
                Uuid::from_slice(&payload.artifact_id).ok()?
            ),
            claims.expected_revision,
        ),
        command_envelope::Payload::AgentUpgrade(payload) => (
            "agent_upgrade_prepare",
            format!(
                "{}:{}",
                payload.target_version,
                hex::encode(&payload.package_sha256)
            ),
            claims.expected_revision,
        ),
        _ => return None,
    };
    Some(CommandEffectBinding {
        effect_kind,
        resource_key,
        effect_revision,
    })
}

pub(crate) fn desired_effect_binding(command: &CommandEnvelope) -> Option<CommandEffectBinding> {
    let (effect_kind, resource_key, effect_revision) = match command.payload.as_ref()? {
        command_envelope::Payload::UserCreate(payload) => (
            "user_create",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserDisable(payload) => (
            "user_disable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserEnable(payload) => (
            "user_enable",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::UserPasswordRotate(payload) => (
            "user_password_rotate",
            payload.username.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::GroupApply(payload) => (
            "group_apply",
            payload.group_name.clone(),
            payload.desired_revision,
        ),
        command_envelope::Payload::CompleteConfigApply(payload) => (
            "config_apply",
            "ocserv.conf".to_owned(),
            payload.desired_revision,
        ),
        _ => return None,
    };
    Some(CommandEffectBinding {
        effect_kind,
        resource_key,
        effect_revision,
    })
}

pub(crate) fn adapter_effect(claims: &CommandAuthorizationV1) -> EffectIdentity<'_> {
    EffectIdentity {
        command_id: &claims.command_id,
        idempotency_key: &claims.idempotency_key,
        semantic_payload_sha256: &claims.semantic_payload_sha256,
        expires_at_unix_seconds: claims.expires_at_seconds,
    }
}

pub(crate) fn authorized_effect<'a>(
    command: &CommandEnvelope,
    claims: &'a CommandAuthorizationV1,
    binding: &'a CommandEffectBinding,
) -> AuthorizedEffectIdentity<'a> {
    AuthorizedEffectIdentity {
        node_id: &claims.node_id,
        command_id: &claims.command_id,
        operation_id: &claims.operation_id,
        idempotency_key: &claims.idempotency_key,
        semantic_payload_sha256: &claims.semantic_payload_sha256,
        action: &claims.action,
        authorization_revision: claims.expected_revision,
        effect_kind: binding.effect_kind,
        resource_key: &binding.resource_key,
        effect_revision: binding.effect_revision,
        expires_at_unix_seconds: claims.expires_at_seconds,
        retry_if_absent: CommandDeliveryMode::try_from(command.delivery_mode)
            .is_ok_and(|mode| mode == CommandDeliveryMode::RetryIfEffectAbsent),
    }
}

pub(crate) fn owner_fence<'a>(
    node_id: &'a [u8; 16],
    endpoint_id: &'a [u8; 32],
    owner_epoch: u64,
) -> OwnerFenceIdentity<'a> {
    OwnerFenceIdentity {
        node_id,
        endpoint_id,
        owner_epoch,
    }
}

pub(crate) fn artifact_identity<'a>(
    claims: &'a ArtifactGrantClaimsV1,
    node_id: &'a [u8; 16],
    endpoint_id: &'a [u8; 32],
    owner_epoch: u64,
) -> ArtifactLeaseIdentity<'a> {
    ArtifactLeaseIdentity {
        artifact_id: &claims.artifact_id,
        certificate_id: &claims.certificate_id,
        certificate_version: claims.certificate_version,
        operation_id: &claims.operation_id,
        authorized_subject: &claims.authorized_subject,
        max_bytes: claims.max_bytes,
        grant_id: &claims.grant_id,
        expires_at_unix_seconds: claims.expires_at_seconds,
        owner_fence: owner_fence(node_id, endpoint_id, owner_epoch),
    }
}

/// Session, ban-removal and reload effects leave no root-verifiable proof that
/// they did not happen, and the Agent's absence report is unsigned. A retry
/// grant for them (including one signed before this rule) therefore never
/// reaches the executor; it can only return an already attested result.
pub(crate) fn fixed_host_effect(command: &CommandEnvelope) -> bool {
    matches!(
        command.payload.as_ref(),
        Some(
            command_envelope::Payload::SessionDisconnect(_)
                | command_envelope::Payload::SessionTerminate(_)
                | command_envelope::Payload::IpBanRemove(_)
                | command_envelope::Payload::ServiceReload(_)
        )
    )
}

pub(crate) fn replay_only_response(
    adapter: &Adapter,
    identity: AuthorizedEffectIdentity<'_>,
    owner_fence: OwnerFenceIdentity<'_>,
) -> PrivdResponse {
    match adapter.replay_authorized_effect_after_fence(identity, owner_fence) {
        Ok(Some(encoded)) => decode_cached_response(&encoded),
        // Missing, pruned or still-prepared evidence cannot prove absence.
        Ok(None) => response_error(
            ErrorKind::Unavailable,
            "authorized effect outcome requires reconciliation",
        ),
        Err(failure) => PrivdResponse {
            request_id: Vec::new(),
            privileged_result_proof: None,
            result: Some(privd_response::Result::Error(PrivdError::from(failure))),
        },
    }
}

pub(crate) fn decode_cached_response(encoded: &[u8]) -> PrivdResponse {
    match PrivdResponse::decode(encoded) {
        Ok(response)
            if response.request_id.is_empty()
                && response.result.is_some()
                && response.privileged_result_proof.is_some() =>
        {
            response
        }
        _ => response_error(
            ErrorKind::Unavailable,
            "cached authorized effect result invalid",
        ),
    }
}

pub(crate) fn response_error(kind: ErrorKind, detail: &'static str) -> PrivdResponse {
    PrivdResponse {
        request_id: Vec::new(),
        privileged_result_proof: None,
        result: Some(privd_response::Result::Error(error(kind, detail))),
    }
}

pub(crate) fn finish_operation(
    operation_name: &str,
    result: Result<privd_response::Result, ocservia_ocserv_adapter::AdapterError>,
) -> privd_response::Result {
    let exit_code = match &result {
        Err(ocservia_ocserv_adapter::AdapterError::CommandFailed { code }) => *code,
        _ => None,
    };
    let result =
        result.unwrap_or_else(|failure| privd_response::Result::Error(PrivdError::from(failure)));
    if let privd_response::Result::Error(mut error) = result {
        tracing::warn!(
            operation = operation_name,
            error_kind = error.kind,
            exit_code,
            "privd operation failed"
        );
        error.detail = format!("{operation_name}: {}", error.detail);
        privd_response::Result::Error(error)
    } else {
        result
    }
}

pub(crate) fn deadline_error() -> privd_response::Result {
    privd_response::Result::Error(error(
        ErrorKind::DeadlineExceeded,
        "request deadline exceeded",
    ))
}

pub(crate) fn error(kind: ErrorKind, detail: &str) -> PrivdError {
    PrivdError {
        kind: kind.into(),
        detail: detail.to_owned(),
    }
}
