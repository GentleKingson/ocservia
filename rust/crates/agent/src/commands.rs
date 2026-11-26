use crate::artifacts::{handle_artifact_consume, handle_artifact_stream};
use crate::session::{
    SessionContext, SessionGrantAuthority, refresh_fence_epoch_floor, validate_artifact_frame_kind,
};
use crate::startup::invalid;
use crate::{ARTIFACT_FRAME_MASK, HANDSHAKE_TIMEOUT};
use ocservia_agent::{
    CommandContext, CommandError, CommandExecutor, CommandOutcome, ExternalCommand,
    ExternalEffectObservation, ExternalPreparation, MAX_COMMAND_BYTES, PrivdClient,
};
use ocservia_agent_protocol::{
    DesiredEffectState, ErrorKind, PrivdResponse, privd_request, privd_response,
};
use ocservia_command_journal::{CommandRecord, CommandState};
use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, CommandDeliveryMode, CommandEnvelope, CommandResult,
    CommandResultState, PrivilegedResultProof, SemanticPayloadHashVersion, command_envelope,
};
use ocservia_contracts::session::{ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME};
use prost::Message;
use std::io;
use std::time::SystemTime;
use uuid::Uuid;

pub(super) async fn handle_command_stream(
    send: iroh::endpoint::SendStream,
    mut recv: iroh::endpoint::RecvStream,
    session: &mut SessionContext<'_>,
    authority: &SessionGrantAuthority,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let mut length = [0_u8; 4];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut length))
        .await
        .map_err(|_| invalid("command length timed out"))??;
    let framed_length = u32::from_be_bytes(length);
    if framed_length & ARTIFACT_FRAME_MASK == ARTIFACT_FETCH_FRAME {
        validate_artifact_frame_kind(framed_length)?;
        return handle_artifact_stream(
            send,
            recv,
            framed_length & !ARTIFACT_FRAME_MASK,
            session.node_id,
            session.command_keys,
            session.privd,
            authority,
            session.command_executor.journal_mut(),
            session.fence_epoch_floor,
        )
        .await;
    }
    if framed_length & ARTIFACT_FRAME_MASK == ARTIFACT_CONSUME_FRAME {
        validate_artifact_frame_kind(framed_length)?;
        return handle_artifact_consume(
            send,
            recv,
            framed_length & !ARTIFACT_FRAME_MASK,
            session.node_id,
            session.command_keys,
            session.privd,
            authority,
            session.command_executor.journal_mut(),
            session.fence_epoch_floor,
        )
        .await;
    }
    if framed_length & ARTIFACT_FRAME_MASK != 0 {
        return Err(invalid("command frame kind invalid").into());
    }
    let length = framed_length as usize;
    if length == 0 || length > MAX_COMMAND_BYTES {
        return Err(invalid("command size invalid").into());
    }
    let mut bytes = vec![0_u8; length];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut bytes))
        .await
        .map_err(|_| invalid("command body timed out"))??;
    let envelope = decode_strict_command_envelope(bytes.as_slice())?;
    let result = process_command(session, authority, &envelope).await?;
    send_command_result(send, &result).await
}

async fn process_command(
    session: &mut SessionContext<'_>,
    authority: &SessionGrantAuthority,
    envelope: &CommandEnvelope,
) -> Result<CommandResult, Box<dyn std::error::Error + Send + Sync>> {
    refresh_fence_epoch_floor(
        session.command_executor.journal_mut(),
        session.fence_epoch_floor,
    )?;
    let now = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH)?;
    let now_unix_seconds = i64::try_from(now.as_secs())?;
    if authority.session_epoch < *session.fence_epoch_floor {
        let result = rejected_result(envelope, "stale_owner_epoch", now_unix_seconds);
        return Ok(result);
    }
    let context = CommandContext {
        node_id: *session.node_id.as_bytes(),
        authorization_revision: authority.authorization_revision,
        capabilities: authority.negotiated_capabilities.clone(),
        session_expires_at_unix_seconds: authority.expires_at_unix_seconds,
        command_keys: session.command_keys.clone(),
        now_unix_seconds,
        cancelled: false,
    };
    let external = matches!(
        envelope.payload,
        Some(
            command_envelope::Payload::SessionDisconnect(_)
                | command_envelope::Payload::SessionTerminate(_)
                | command_envelope::Payload::IpBanRemove(_)
                | command_envelope::Payload::ServiceReload(_)
                | command_envelope::Payload::UserCreate(_)
                | command_envelope::Payload::UserDisable(_)
                | command_envelope::Payload::UserEnable(_)
                | command_envelope::Payload::UserPasswordRotate(_)
                | command_envelope::Payload::GroupApply(_)
                | command_envelope::Payload::CompleteConfigPlan(_)
                | command_envelope::Payload::CompleteConfigApply(_)
                | command_envelope::Payload::CertificateCsr(_)
                | command_envelope::Payload::CertificateRevoke(_)
                | command_envelope::Payload::CertificateP12(_)
                | command_envelope::Payload::AgentUpgrade(_)
        )
    );
    tracing::info!(
        event_type = "command_delivery_authorized",
        message_id = %hex::encode(&envelope.message_id),
        command_id = %hex::encode(&envelope.command_id),
        node_id = %hex::encode(&envelope.node_id),
        delivery_mode = envelope.delivery_mode,
        semantic_payload_sha256 = %hex::encode(&envelope.semantic_payload_sha256),
        semantic_payload_hash_version = envelope.semantic_payload_hash_version,
        "command delivery passed session authorization"
    );
    let execution = if external {
        execute_external_command(session, authority, envelope, &context, now_unix_seconds).await
    } else {
        session.command_executor.deliver(envelope, &context)
    };
    let result = delivery_result(execution, envelope, now_unix_seconds);
    tracing::info!(
        event_type = "command_delivery_result",
        message_id = %hex::encode(&envelope.message_id),
        command_id = %hex::encode(&envelope.command_id),
        delivery_mode = envelope.delivery_mode,
        state = result.state,
        replayed = result.replayed,
        error_code = %result.error_code,
        "command delivery produced a result"
    );
    Ok(result)
}

fn delivery_result(
    execution: Result<CommandOutcome, CommandError>,
    envelope: &CommandEnvelope,
    now_unix_seconds: i64,
) -> CommandResult {
    match execution {
        Ok(outcome) => command_result(&outcome.record, outcome.replayed),
        Err(CommandError::Rejected(code)) => rejected_result(envelope, code, now_unix_seconds),
        Err(CommandError::IdentityConflict) => {
            rejected_result(envelope, "command_identity_conflict", now_unix_seconds)
        }
        Err(CommandError::PayloadConflict) => {
            rejected_result(envelope, "idempotency_payload_conflict", now_unix_seconds)
        }
        Err(CommandError::PreEffectJournalFailure(error)) => {
            tracing::error!(command_id = %hex::encode(&envelope.command_id), error = %error, "command journal failure");
            rejected_result(envelope, "journal_unavailable", now_unix_seconds)
        }
        Err(CommandError::OutcomeUnknown {
            code,
            record,
            source,
        }) => {
            tracing::error!(command_id = %hex::encode(&envelope.command_id), error = %source, "command outcome requires reconciliation");
            unknown_result(record.as_ref(), code)
        }
        Err(CommandError::InjectedCrash(_)) => unreachable!("crash injection is test-only"),
    }
}

async fn send_command_result(
    mut send: iroh::endpoint::SendStream,
    result: &CommandResult,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let event = AgentEvent {
        r#type: AgentEventType::CommandResult.into(),
        payload: result.encode_to_vec(),
    };
    let encoded = event.encode_to_vec();
    send.write_all(&u32::try_from(encoded.len())?.to_be_bytes())
        .await?;
    send.write_all(&encoded).await?;
    send.finish()?;
    Ok(())
}

async fn execute_external_command(
    session: &mut SessionContext<'_>,
    authority: &SessionGrantAuthority,
    envelope: &CommandEnvelope,
    context: &CommandContext,
    now: i64,
) -> Result<CommandOutcome, CommandError> {
    let mode = CommandDeliveryMode::try_from(envelope.delivery_mode)
        .unwrap_or(CommandDeliveryMode::Unspecified);
    let (command, reconciled_response) = match mode {
        CommandDeliveryMode::ExecuteOrReplay => {
            match session
                .command_executor
                .prepare_external(envelope, context)?
            {
                ExternalPreparation::Execute(command) => (command, None),
                ExternalPreparation::Replay(outcome) => return Ok(outcome),
            }
        }
        CommandDeliveryMode::ReconcileOnly => {
            let response = session.privd.call_reconcile(envelope).await;
            // A present proof goes through settlement even when malformed. Its
            // result cannot serve as proofless evidence that the effect is absent.
            if response
                .as_ref()
                .is_ok_and(|response| response.privileged_result_proof.is_some())
            {
                match session
                    .command_executor
                    .begin_attested_recovery(envelope, context)?
                {
                    ExternalPreparation::Execute(command) => (command, Some(response)),
                    ExternalPreparation::Replay(outcome) => return Ok(outcome),
                }
            } else {
                let observed =
                    observe_external_effect_with_reconcile(session.privd, envelope, Some(response))
                        .await;
                return session
                    .command_executor
                    .reconcile_external(envelope, context, observed);
            }
        }
        CommandDeliveryMode::RetryIfEffectAbsent => (
            session.command_executor.retry_external(envelope, context)?,
            None,
        ),
        CommandDeliveryMode::Unspecified => {
            return Err(CommandError::Rejected("delivery_mode_invalid"));
        }
    };
    let response = match reconciled_response {
        Some(response) => response,
        None => {
            session
                .privd
                .call_command(envelope, command.accepted_at(), &authority.raw_grant)
                .await
        }
    };
    match response {
        Ok(response) => {
            settle_privd_response(session.command_executor, &command, envelope, response, now)
        }
        Err(_) => {
            session
                .command_executor
                .mark_external_unknown(&command, "privd_transport_unknown", now)
        }
    }
}

pub(super) fn settle_privd_response(
    executor: &mut CommandExecutor,
    command: &ExternalCommand,
    envelope: &CommandEnvelope,
    response: PrivdResponse,
    now: i64,
) -> Result<CommandOutcome, CommandError> {
    let Some((proof, completed_at)) = response_proof(&response) else {
        let code = if response.privileged_result_proof.is_some() {
            "receipt_invalid"
        } else {
            "receipt_missing"
        };
        return executor.mark_external_unknown(command, code, now);
    };
    let mut applied_revision = None;
    let terminal = match response.result {
        Some(privd_response::Result::Mutation(result)) if result.applied => {
            applied_revision = desired_resource(envelope);
            Ok(Vec::new())
        }
        Some(privd_response::Result::ConfigPlan(result)) => Ok(result.encode_to_vec()),
        Some(privd_response::Result::ConfigApply(result)) => {
            if !result.rolled_back && !result.failed_critical && result.applied_revision != 0 {
                applied_revision = Some(
                    desired_resource(envelope)
                        .ok_or(CommandError::Rejected("config_apply_invalid"))?,
                );
            }
            Ok(result.encode_to_vec())
        }
        Some(privd_response::Result::CertificateCsr(result)) => Ok(result.encode_to_vec()),
        Some(privd_response::Result::CertificateRevoke(result)) => Ok(result.encode_to_vec()),
        Some(privd_response::Result::CertificateP12(result)) => Ok(result.encode_to_vec()),
        Some(privd_response::Result::AgentUpgradeScheduled(result)) => Ok(result.encode_to_vec()),
        Some(privd_response::Result::Error(error)) => {
            let Some(code) = terminal_privd_error_code(error.kind) else {
                return executor.mark_external_unknown(command, "privd_outcome_unknown", now);
            };
            Err(code)
        }
        _ => {
            return executor.mark_external_unknown(command, "privd_outcome_unknown", now);
        }
    };
    if let (Some((resource_type, resource_key, revision)), Ok(bytes)) =
        (applied_revision, &terminal)
    {
        return executor.complete_external_applied_attested(
            command,
            resource_type,
            &resource_key,
            revision,
            bytes,
            &proof,
            completed_at,
        );
    }
    executor.complete_external_attested(
        command,
        terminal.as_deref().map_err(|code| *code),
        &proof,
        completed_at,
    )
}

pub(super) fn response_proof(response: &PrivdResponse) -> Option<(Vec<u8>, i64)> {
    let proof = response.privileged_result_proof.as_ref()?;
    if proof.version != 1 || proof.signature.len() != 64 {
        return None;
    }
    let receipt = proof.receipt_v1.as_ref()?;
    if receipt.receipt_version != 1 || receipt.completed_at.as_ref()?.seconds < 0 {
        return None;
    }
    let encoded = proof.encode_to_vec();
    (encoded.len() <= 65_536).then_some((encoded, receipt.completed_at.as_ref()?.seconds))
}

pub(super) fn terminal_privd_error_code(kind: i32) -> Option<&'static str> {
    match ErrorKind::try_from(kind).unwrap_or(ErrorKind::Unspecified) {
        ErrorKind::CapacityExceeded => Some("capacity_exceeded"),
        ErrorKind::InvalidRequest | ErrorKind::PermissionDenied | ErrorKind::MalformedOutput => {
            Some("privd_rejected")
        }
        _ => None,
    }
}

fn desired_resource(envelope: &CommandEnvelope) -> Option<(&'static str, String, u64)> {
    match envelope.payload.as_ref()? {
        command_envelope::Payload::UserCreate(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::UserDisable(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::UserEnable(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::UserPasswordRotate(value) => {
            Some(("user", value.username.clone(), value.desired_revision))
        }
        command_envelope::Payload::GroupApply(value) => {
            Some(("group", value.group_name.clone(), value.desired_revision))
        }
        command_envelope::Payload::CompleteConfigApply(value) => {
            Some(("config", "ocserv.conf".to_owned(), value.desired_revision))
        }
        command_envelope::Payload::CertificateCsr(value) => {
            Uuid::from_slice(&value.certificate_id).ok().map(|id| {
                (
                    "certificate_key",
                    id.to_string(),
                    envelope.expected_revision,
                )
            })
        }
        command_envelope::Payload::CertificateRevoke(value) => {
            Uuid::from_slice(&value.certificate_id).ok().map(|id| {
                (
                    "certificate_revoke",
                    id.to_string(),
                    envelope.expected_revision,
                )
            })
        }
        command_envelope::Payload::CertificateP12(value) => {
            Uuid::from_slice(&value.artifact_id).ok().map(|id| {
                (
                    "certificate_artifact",
                    id.to_string(),
                    envelope.expected_revision,
                )
            })
        }
        _ => None,
    }
}

pub(super) async fn observe_external_effect_with_reconcile(
    privd: &PrivdClient,
    envelope: &CommandEnvelope,
    prefetched_reconcile: Option<Result<PrivdResponse, io::Error>>,
) -> ExternalEffectObservation {
    if matches!(
        envelope.payload,
        Some(command_envelope::Payload::CompleteConfigPlan(_))
    ) {
        return ExternalEffectObservation::Absent;
    }
    if matches!(
        envelope.payload,
        Some(
            command_envelope::Payload::CertificateCsr(_)
                | command_envelope::Payload::CertificateRevoke(_)
                | command_envelope::Payload::CertificateP12(_)
        )
    ) {
        return ExternalEffectObservation::Absent;
    }
    let target_boot = match envelope.payload.as_ref() {
        Some(command_envelope::Payload::SessionDisconnect(target)) => Some(&target.boot_id),
        Some(command_envelope::Payload::SessionTerminate(target)) => Some(&target.boot_id),
        _ => None,
    };
    if let Some(target_boot) = target_boot {
        let Ok(current_boot) = tokio::fs::read_to_string("/proc/sys/kernel/random/boot_id").await
        else {
            return ExternalEffectObservation::Unknown;
        };
        if current_boot.trim() != target_boot {
            return ExternalEffectObservation::Unknown;
        }
    }
    let Some(payload) = envelope.payload.as_ref() else {
        return ExternalEffectObservation::Unknown;
    };
    let response = match payload {
        command_envelope::Payload::SessionDisconnect(_)
        | command_envelope::Payload::SessionTerminate(_) => {
            privd
                .call(privd_request::Operation::SessionList(
                    ocservia_agent_protocol::ReadRequest {},
                ))
                .await
        }
        command_envelope::Payload::IpBanRemove(_) => {
            privd
                .call(privd_request::Operation::IpBanList(
                    ocservia_agent_protocol::ReadRequest {},
                ))
                .await
        }
        payload if desired_effect_identity(payload).is_some() => match prefetched_reconcile {
            Some(response) => response,
            None => privd.call_reconcile(envelope).await,
        },
        _ => return ExternalEffectObservation::Unknown,
    };
    let Ok(response) = response else {
        return ExternalEffectObservation::Unknown;
    };
    let Some(result) = response.result else {
        return ExternalEffectObservation::Unknown;
    };
    map_external_effect(payload, result)
}

pub(super) fn map_external_effect(
    payload: &command_envelope::Payload,
    result: privd_response::Result,
) -> ExternalEffectObservation {
    match (payload, result) {
        (
            command_envelope::Payload::SessionDisconnect(target),
            privd_response::Result::SessionList(current),
        ) => {
            if current
                .sessions
                .iter()
                .any(|session| session.id == target.session_id)
            {
                ExternalEffectObservation::Absent
            } else {
                ExternalEffectObservation::AppliedExact
            }
        }
        (
            command_envelope::Payload::SessionTerminate(target),
            privd_response::Result::SessionList(current),
        ) => {
            if current
                .sessions
                .iter()
                .any(|session| session.id == target.session_id)
            {
                ExternalEffectObservation::Absent
            } else {
                ExternalEffectObservation::AppliedExact
            }
        }
        (
            command_envelope::Payload::IpBanRemove(target),
            privd_response::Result::IpBanList(current),
        ) => {
            if current.bans.iter().any(|ban| ban.ip == target.ip) {
                ExternalEffectObservation::Absent
            } else {
                ExternalEffectObservation::AppliedExact
            }
        }
        (payload, privd_response::Result::DesiredEffectObservation(observation))
            if desired_effect_identity(payload).is_some() =>
        {
            match DesiredEffectState::try_from(observation.state)
                .unwrap_or(DesiredEffectState::Unspecified)
            {
                DesiredEffectState::AppliedExact => ExternalEffectObservation::AppliedExact,
                DesiredEffectState::SupersededByNewerRevision => {
                    ExternalEffectObservation::SupersededByNewerRevision
                }
                DesiredEffectState::Absent => ExternalEffectObservation::Absent,
                DesiredEffectState::Unknown | DesiredEffectState::Unspecified => {
                    ExternalEffectObservation::Unknown
                }
            }
        }
        _ => ExternalEffectObservation::Unknown,
    }
}

fn desired_effect_identity(
    payload: &command_envelope::Payload,
) -> Option<(&'static str, &str, u64)> {
    match payload {
        command_envelope::Payload::UserCreate(value) => {
            Some(("user_create", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserDisable(value) => {
            Some(("user_disable", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserEnable(value) => {
            Some(("user_enable", &value.username, value.desired_revision))
        }
        command_envelope::Payload::UserPasswordRotate(value) => Some((
            "user_password_rotate",
            &value.username,
            value.desired_revision,
        )),
        command_envelope::Payload::GroupApply(value) => {
            Some(("group_apply", &value.group_name, value.desired_revision))
        }
        command_envelope::Payload::CompleteConfigApply(value) => {
            Some(("config_apply", "ocserv.conf", value.desired_revision))
        }
        _ => None,
    }
}

fn unknown_result(record: &CommandRecord, code: &str) -> CommandResult {
    let mut result = command_result(record, false);
    result.state = CommandResultState::Unknown.into();
    result.result.clear();
    code.clone_into(&mut result.error_code);
    result
}

fn command_result(record: &CommandRecord, replayed: bool) -> CommandResult {
    let state = match record.state {
        CommandState::Succeeded => CommandResultState::Succeeded,
        CommandState::Failed => CommandResultState::Failed,
        CommandState::Unknown | CommandState::Accepted | CommandState::Running => {
            CommandResultState::Unknown
        }
    };
    let proof = record
        .privileged_result_proof
        .as_deref()
        .and_then(|encoded| PrivilegedResultProof::decode(encoded).ok());
    let receipt = proof.as_ref().and_then(|value| value.receipt_v1.as_ref());
    CommandResult {
        command_id: record.command_id.to_vec(),
        idempotency_key: record.idempotency_key.to_vec(),
        payload_sha256: record.payload_sha256.to_vec(),
        state: state.into(),
        result: record.result.clone().unwrap_or_default(),
        error_code: record.error_code.clone().unwrap_or_default(),
        accepted_at: receipt
            .and_then(|value| value.accepted_at)
            .or(Some(prost_types::Timestamp {
                seconds: record.accepted_at,
                nanos: 0,
            })),
        completed_at: receipt.and_then(|value| value.completed_at).or(Some(
            prost_types::Timestamp {
                seconds: record.updated_at,
                nanos: 0,
            },
        )),
        replayed: receipt.is_some_and(|value| value.replayed) || proof.is_none() && replayed,
        semantic_payload_hash_version: record.payload_hash_version,
        privileged_result_proof: proof,
    }
}

fn rejected_result(envelope: &CommandEnvelope, code: &str, now: i64) -> CommandResult {
    CommandResult {
        command_id: envelope.command_id.clone(),
        idempotency_key: envelope.idempotency_key.clone(),
        payload_sha256: Vec::new(),
        state: CommandResultState::Rejected.into(),
        result: Vec::new(),
        error_code: code.to_owned(),
        accepted_at: None,
        completed_at: Some(prost_types::Timestamp {
            seconds: now,
            nanos: 0,
        }),
        replayed: false,
        semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
        privileged_result_proof: None,
    }
}

#[cfg(test)]
mod tests {
    use ed25519_dalek::SigningKey;
    use ocservia_agent_protocol::{
        DesiredEffectObservation, MutationResult, PrivdRequest, PrivilegedRequestMode, read_frame,
        write_frame,
    };
    use ocservia_command_journal::Journal;
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        GroupApply, ServiceReload, SessionGrantV1,
    };
    use ocservia_test_support::{keyring, sign_command_envelope_v2, sign_command_v2};
    use std::time::Duration;

    use super::*;

    #[test]
    fn settlement_publishes_canonical_receipt_failure_codes() {
        let directory = std::env::temp_dir().join(format!("ocservia-receipt-{}", Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("test directory");
        let journal = Journal::open(&directory.join("journal.sqlite3")).expect("test journal");
        let mut executor = CommandExecutor::new(journal);
        let signing = SigningKey::from_bytes(&[49; 32]);
        let now = 1_700_000_050;
        let node_id = *Uuid::now_v7().as_bytes();
        let context = CommandContext {
            node_id,
            authorization_revision: 1,
            capabilities: ["ocserv.service.reload".to_owned()].into(),
            session_expires_at_unix_seconds: now + 300,
            command_keys: keyring(&signing),
            now_unix_seconds: now,
            cancelled: false,
        };
        for (proof, code) in [
            (None, "receipt_missing"),
            (Some(PrivilegedResultProof::default()), "receipt_invalid"),
        ] {
            let envelope = sign_command_v2(
                &signing,
                node_id,
                now,
                now + 60,
                context.authorization_revision,
                "service.reload",
                "ocserv.service.reload",
                command_envelope::Payload::ServiceReload(ServiceReload {}),
            );
            let ExternalPreparation::Execute(command) = executor
                .prepare_external(&envelope, &context)
                .expect("prepare")
            else {
                panic!("new command should execute");
            };
            let response = PrivdResponse {
                result: Some(privd_response::Result::Mutation(MutationResult {
                    applied: true,
                })),
                privileged_result_proof: proof,
                ..PrivdResponse::default()
            };
            let outcome =
                settle_privd_response(&mut executor, &command, &envelope, response, now + 1)
                    .expect("persist unknown result");
            let result = command_result(&outcome.record, outcome.replayed);
            assert_eq!(result.state, i32::from(CommandResultState::Unknown));
            assert_eq!(result.error_code, code);
            assert!(result.privileged_result_proof.is_none());
        }
        drop(executor);
        std::fs::remove_dir_all(directory).expect("cleanup journal");
    }

    #[tokio::test]
    async fn reconciliation_rejects_malformed_receipts_before_using_effect_observation() {
        let directory = std::env::temp_dir().join(format!("ocservia-reconcile-{}", Uuid::now_v7()));
        std::fs::create_dir(&directory).expect("test directory");
        let mut executor = CommandExecutor::new(
            Journal::open(&directory.join("journal.sqlite3")).expect("test journal"),
        );
        let signing = SigningKey::from_bytes(&[49; 32]);
        let now = 1_700_000_050;
        let node_id = Uuid::now_v7();
        let context = CommandContext {
            node_id: *node_id.as_bytes(),
            authorization_revision: 1,
            capabilities: ["ocserv.groups.write".to_owned()].into(),
            session_expires_at_unix_seconds: now + 300,
            command_keys: keyring(&signing),
            now_unix_seconds: now,
            cancelled: false,
        };
        let socket = directory.join("privd.sock");
        let listener = tokio::net::UnixListener::bind(&socket).expect("test socket");
        let privd = PrivdClient::new(socket, Duration::from_secs(1)).expect("privd client");
        let server = tokio::spawn(async move {
            for proof in [None, Some(PrivilegedResultProof::default())] {
                let (mut stream, _) = listener.accept().await.expect("test connection");
                let request: PrivdRequest =
                    read_frame(&mut stream).await.expect("reconcile request");
                assert_eq!(
                    request.privileged_mode,
                    i32::from(PrivilegedRequestMode::Reconcile)
                );
                write_frame(
                    &mut stream,
                    &PrivdResponse {
                        request_id: request.request_id,
                        result: Some(privd_response::Result::DesiredEffectObservation(
                            DesiredEffectObservation {
                                state: DesiredEffectState::Absent.into(),
                                observed_revision: 1,
                            },
                        )),
                        privileged_result_proof: proof,
                    },
                )
                .await
                .expect("reconcile response");
            }
        });
        let mut fence_epoch_floor = 1;
        let mut session = SessionContext {
            node_id,
            endpoint_id: iroh::SecretKey::generate().public(),
            privd: &privd,
            command_executor: &mut executor,
            host_metrics: crate::host_metrics::HostMetrics::default(),
            session_cache: crate::telemetry::SessionCache::default(),
            boot_id: "",
            os_release: "",
            agent_instance_id: Uuid::now_v7(),
            command_keys: &context.command_keys,
            sealing_keys: &[],
            fence_epoch_floor: &mut fence_epoch_floor,
        };
        let authority = SessionGrantAuthority {
            raw_grant: SessionGrantV1::default(),
            negotiated_capabilities: context.capabilities.clone(),
            authorization_revision: context.authorization_revision,
            session_epoch: 1,
            expires_at_unix_seconds: context.session_expires_at_unix_seconds,
        };
        for code in ["effect_absent", "receipt_invalid"] {
            let mut envelope = sign_command_v2(
                &signing,
                *node_id.as_bytes(),
                now,
                now + 60,
                context.authorization_revision,
                "group.apply",
                "ocserv.groups.write",
                command_envelope::Payload::GroupApply(GroupApply {
                    group_name: "staff".to_owned(),
                    members: vec!["alice".to_owned()],
                    desired_revision: 1,
                }),
            );
            envelope.delivery_mode = CommandDeliveryMode::ReconcileOnly.into();
            sign_command_envelope_v2(&signing, &mut envelope);
            let outcome =
                execute_external_command(&mut session, &authority, &envelope, &context, now + 1)
                    .await
                    .expect("record reconciliation outcome");
            assert_eq!(outcome.record.state, CommandState::Unknown);
            assert_eq!(outcome.record.error_code.as_deref(), Some(code));
        }
        server.await.expect("test server");
        drop(executor);
        std::fs::remove_dir_all(directory).expect("cleanup journal");
    }
}
