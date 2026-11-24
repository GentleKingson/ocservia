use std::time::SystemTime;

use iroh::endpoint::Connection;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, CommandDeliveryMode, CommandEnvelope, CommandResult,
    CommandResultState, command_envelope,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    TransportEvent, TransportEventType,
};
use prost::Message;
use tonic::Status;
use uuid::Uuid;

use crate::{MAX_FRAME_BYTES, STREAM_TIMEOUT, Shared};

#[allow(clippy::too_many_lines)]
pub(crate) async fn read_agent_events(
    mut recv: iroh::endpoint::RecvStream,
    shared: Shared,
    node_id: Vec<u8>,
    max_message_size: usize,
    connection: Connection,
    response_deadline: tokio::time::Instant,
    command: CommandEnvelope,
) {
    let traceparent = command.traceparent.clone();
    let event_context = (
        &shared,
        node_id.as_slice(),
        traceparent.as_str(),
        &connection,
    );
    let mut length = [0_u8; 4];
    match tokio::time::timeout_at(response_deadline, recv.read_exact(&mut length)).await {
        Ok(Ok(())) => {}
        Ok(Err(_)) => {
            publish_command_unknown(
                event_context,
                &command,
                "agent response ended before a terminal event",
            )
            .await;
            return;
        }
        Err(_) => {
            publish_command_unknown(event_context, &command, "agent response timed out").await;
            return;
        }
    }
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > max_message_size || length > MAX_FRAME_BYTES {
        publish_command_unknown(event_context, &command, "agent response size invalid").await;
        return;
    }
    let mut bytes = vec![0_u8; length];
    if !matches!(
        tokio::time::timeout_at(response_deadline, recv.read_exact(&mut bytes)).await,
        Ok(Ok(()))
    ) {
        publish_command_unknown(event_context, &command, "agent response body invalid").await;
        return;
    }
    let Ok(agent_event) = AgentEvent::decode(bytes.as_slice()) else {
        publish_command_unknown(event_context, &command, "agent response protobuf invalid").await;
        return;
    };
    let event_type = match AgentEventType::try_from(agent_event.r#type) {
        Ok(AgentEventType::CommandResult) => TransportEventType::CommandResult,
        Ok(AgentEventType::Error) => TransportEventType::Error,
        _ => {
            publish_command_unknown(event_context, &command, "agent response type invalid").await;
            return;
        }
    };
    if event_type == TransportEventType::CommandResult {
        let Ok(result) = CommandResult::decode(agent_event.payload.as_slice()) else {
            publish_command_unknown(
                event_context,
                &command,
                "Agent command result protobuf invalid",
            )
            .await;
            return;
        };
        if result.command_id != command.command_id {
            publish_command_unknown(
                event_context,
                &command,
                "Agent command result names a different command",
            )
            .await;
            return;
        }
        if let Some(code) = privileged_result_proof_error(&command, &result) {
            publish_command_unknown_with_code(
                event_context,
                &command,
                "privd result receipt missing or malformed",
                code,
            )
            .await;
            return;
        }
        tracing::info!(
            event_type = "command_response_received",
            message_id = %hex::encode(&command.message_id),
            command_id = %hex::encode(&result.command_id),
            node_id = %hex::encode(&node_id),
            delivery_mode = command.delivery_mode,
            state = result.state,
            replayed = result.replayed,
            "Agent command result received"
        );
    }
    publish_agent_event(
        event_context,
        event_with_traceparent(
            &node_id,
            connection.remote_id().as_bytes(),
            event_type,
            agent_event.payload,
            &traceparent,
        ),
    )
    .await;
}

async fn publish_command_unknown(
    context: (&Shared, &[u8], &str, &Connection),
    command: &CommandEnvelope,
    message: &'static str,
) {
    publish_command_unknown_with_code(context, command, message, "outcome_requires_reconciliation")
        .await;
}

async fn publish_command_unknown_with_code(
    (shared, node_id, traceparent, connection): (&Shared, &[u8], &str, &Connection),
    command: &CommandEnvelope,
    message: &'static str,
    code: &'static str,
) {
    tracing::warn!(
        event_type = "command_response_unknown",
        response_reason = message,
        message_id = %hex::encode(&command.message_id),
        command_id = %hex::encode(&command.command_id),
        node_id = %hex::encode(node_id),
        delivery_mode = command.delivery_mode,
        "command response requires reconciliation"
    );
    let now = now_timestamp();
    let result = CommandResult {
        command_id: command.command_id.clone(),
        idempotency_key: command.idempotency_key.clone(),
        payload_sha256: command.semantic_payload_sha256.clone(),
        state: CommandResultState::Unknown.into(),
        result: Vec::new(),
        error_code: code.to_owned(),
        accepted_at: Some(now),
        completed_at: Some(now),
        replayed: false,
        semantic_payload_hash_version: command.semantic_payload_hash_version,
        privileged_result_proof: None,
    };
    publish_agent_event(
        (shared, node_id, traceparent, connection),
        event_with_traceparent(
            node_id,
            connection.remote_id().as_bytes(),
            TransportEventType::CommandResult,
            result.encode_to_vec(),
            traceparent,
        ),
    )
    .await;
}

pub(crate) fn privileged_result_proof_error(
    command: &CommandEnvelope,
    result: &CommandResult,
) -> Option<&'static str> {
    let state = CommandResultState::try_from(result.state);
    let terminal = matches!(
        state,
        Ok(CommandResultState::Succeeded
            | CommandResultState::Failed
            | CommandResultState::Rejected)
    );
    let privileged = !matches!(
        command.payload,
        Some(
            command_envelope::Payload::SimulationProbe(_)
                | command_envelope::Payload::SyntheticNoop(_)
                | command_envelope::Payload::SyntheticEcho(_)
        ) | None
    );
    if !terminal || !privileged {
        return None;
    }
    let Some(proof) = result.privileged_result_proof.as_ref() else {
        return Some("receipt_missing");
    };
    let Some(receipt) = proof.receipt_v1.as_ref() else {
        return Some("receipt_invalid");
    };
    let valid = state != Ok(CommandResultState::Rejected)
        && proof.version == 1
        && receipt.receipt_version == 1
        && proof.signature.len() == 64
        && proof.encoded_len() <= 65_536
        && receipt.node_id.len() == 16
        && receipt.command_id == command.command_id
        && receipt.operation_id.len() == 16
        && receipt.idempotency_key.len() == 16
        && receipt.semantic_payload_sha256.len() == 32
        && receipt.result_cbor_sha256.len() == 32
        && receipt.error_code.len() <= 128
        && receipt.effect_sequence > 0
        && receipt.completed_at.is_some()
        && receipt.accepted_at.is_some();
    (!valid).then_some("receipt_invalid")
}

pub(crate) fn command_response_deadline(
    command: &CommandEnvelope,
) -> Result<tokio::time::Instant, Status> {
    let expires_at = command
        .expires_at
        .as_ref()
        .ok_or_else(|| Status::invalid_argument("command expires_at is required"))?;
    let expires_at = SystemTime::try_from(*expires_at)
        .map_err(|_| Status::invalid_argument("command expires_at is invalid"))?;
    if command.delivery_mode == i32::from(CommandDeliveryMode::ReconcileOnly) {
        return Ok(tokio::time::Instant::now() + STREAM_TIMEOUT);
    }
    let remaining = expires_at
        .duration_since(SystemTime::now())
        .map_err(|_| Status::deadline_exceeded("command has expired"))?;
    Ok(tokio::time::Instant::now() + remaining)
}

async fn publish_agent_event(
    (shared, node_id, _, connection): (&Shared, &[u8], &str, &Connection),
    event: TransportEvent,
) {
    let mut registry = shared.inner.connections.lock().await;
    if let Some(entry) = registry
        .get_mut(node_id)
        .filter(|entry| entry.connection.stable_id() == connection.stable_id())
    {
        entry.metadata.last_seen = Some(now_timestamp());
        shared.publish(event).await;
    }
    drop(registry);
}

pub(crate) fn event(
    node_id: &[u8],
    endpoint_id: &[u8],
    kind: TransportEventType,
    payload: Vec<u8>,
) -> TransportEvent {
    event_with_traceparent(node_id, endpoint_id, kind, payload, &new_traceparent())
}

/// Builds an event carrying the connection ID and monotonic `session_epoch`
/// so the Controller can correlate session lifecycle transitions cleanly.
pub(crate) fn event_with_session(
    node_id: &[u8],
    endpoint_id: &[u8],
    kind: TransportEventType,
    payload: Vec<u8>,
    connection_id: &[u8],
    session_epoch: u64,
) -> TransportEvent {
    let mut transport_event = event(node_id, endpoint_id, kind, payload);
    transport_event.connection_id = connection_id.to_vec();
    transport_event.session_epoch = session_epoch;
    transport_event
}

fn event_with_traceparent(
    node_id: &[u8],
    endpoint_id: &[u8],
    kind: TransportEventType,
    payload: Vec<u8>,
    traceparent: &str,
) -> TransportEvent {
    TransportEvent {
        event_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        r#type: kind.into(),
        occurred_at: Some(now_timestamp()),
        payload,
        traceparent: traceparent.to_owned(),
        endpoint_id: endpoint_id.to_vec(),
        connection_id: Vec::new(),
        session_epoch: 0,
    }
}

fn is_lower_hex(bytes: &[u8]) -> bool {
    bytes
        .iter()
        .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(byte))
}

pub(crate) fn validate_traceparent(value: &str) -> Result<(), Status> {
    let bytes = value.as_bytes();
    if bytes.len() != 55
        || &bytes[0..3] != b"00-"
        || bytes[35] != b'-'
        || bytes[52] != b'-'
        || !is_lower_hex(&bytes[3..35])
        || !is_lower_hex(&bytes[36..52])
        || !is_lower_hex(&bytes[53..55])
        || bytes[3..35].iter().all(|&b| b == b'0')
        || bytes[36..52].iter().all(|&b| b == b'0')
    {
        return Err(Status::invalid_argument("invalid traceparent"));
    }
    Ok(())
}

pub(crate) fn new_traceparent() -> String {
    let trace_id = Uuid::now_v7();
    let span_id = Uuid::now_v7();
    format!(
        "00-{}-{}-01",
        hex::encode(trace_id.as_bytes()),
        hex::encode(&span_id.as_bytes()[..8])
    )
}

pub(crate) fn now_timestamp() -> prost_types::Timestamp {
    SystemTime::now().into()
}
