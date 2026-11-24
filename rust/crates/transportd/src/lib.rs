//! Iroh endpoint ownership, protocol routing, and the typed UDS transport service.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet};
use std::pin::Pin;
use std::sync::{Arc, Weak};
use std::time::{Duration, SystemTime};

mod admission;
mod artifacts;
mod command_stream;
mod events;
mod fence;
mod monitors;
mod registry;
mod trust;

use admission::SecurityHook;
#[cfg(test)]
use admission::{AttemptRejection, record_attempt, record_global_attempt};
use artifacts::{artifact_stream_deadline, finalize_artifact_consumption, relay_artifact_stream};
use command_stream::{command_response_deadline, read_agent_events, validate_traceparent};
#[cfg(test)]
use command_stream::{event, new_traceparent, now_timestamp, privileged_result_proof_error};
use events::EventState;
#[cfg(test)]
use fence::{FenceRegistration, fence_registration_decision, unix_now};
use fence::{
    RegisteredFence, fixed16, lease_expired_at, same_fence_term, unix_now_parts, validate_uuid,
    verify_command_carriers,
};
use futures_util::StreamExt as _;
use iroh::endpoint::{
    AfterHandshakeOutcome, Connection, EndpointHooks, QuicTransportConfig, RelayMode, VarInt,
    presets,
};
use iroh::protocol::{AcceptError, Router};
use iroh::{Endpoint, EndpointId, SecretKey};
use monitors::{ProtocolKind, SessionHandler, metadata_path, protocol_error};
#[cfg(test)]
use monitors::{
    validate_enrollment, validate_handshake, validate_negotiated_session, validate_protocol_version,
};
use ocservia_command_authorization::ControllerCommandKeyring;
use ocservia_contracts::decode_strict_command_envelope;
#[cfg(test)]
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, CommandResult, CommandResultState, ConnectionFenceV2,
    EnrollRequest, EnrollResponse, HandshakeResult, SessionHandshake, SessionHandshakeResponse,
    TelemetryBatch, command_envelope,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactChunk, ArtifactConsumeRequest as AgentArtifactConsumeRequest,
    ArtifactConsumeResponse as AgentArtifactConsumeResponse, ArtifactFetchRequest, ArtifactGrantV1,
    CommandEnvelope, FenceBindingV2, FenceOperationKind,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    CloseNodeRequest, CloseNodeResponse, ConnectionPath, ConsumeArtifactRequest,
    ConsumeArtifactResponse, FetchArtifactRequest, GetNodeConnectionRequest, GetOwnerFenceRequest,
    GetOwnerFenceResponse, HealthRequest, HealthResponse, HealthStatus, NodeConnection,
    NodeTrustState, RegisterOwnerFenceRequest, RegisterOwnerFenceResponse, SendCommandRequest,
    SendCommandResponse, TransportEvent, TrustUpdateDisposition, UpdateNodeTrustRequest,
    UpdateNodeTrustResponse, WatchEventsRequest, transport_service_server::TransportService,
};
#[cfg(test)]
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    OwnerFenceDisposition, TransportEventType,
};
use ocservia_contracts::session::{ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME};
use prost::Message;
use tokio::sync::{Mutex, Notify, Semaphore, mpsc, watch};
use tokio_stream::{Stream, wrappers::ReceiverStream};
use tonic::{Request, Response, Status};
use trust::PolicyUpdateDisposition;
#[cfg(test)]
use trust::TrustBudgetSet;
pub use trust::{
    IdentityPolicy, TrustAuthority, TrustBudgetConfig, TrustClass, TrustClassLimit,
    TrustMetricsSnapshot,
};

#[cfg(test)]
use std::collections::VecDeque;
#[cfg(test)]
use std::sync::atomic::{AtomicU64, Ordering};
#[cfg(test)]
use std::time::Instant;
#[cfg(test)]
use uuid::Uuid;

/// ALPN for one-time enrollment connections.
pub const ENROLL_ALPN: &[u8] = b"ocserv-platform/enroll/1";
/// ALPN for approved agent sessions.
pub const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";

const PROTOCOL_MAJOR: u32 = 1;
const PROTOCOL_MINOR: u32 = 1;
const MAX_HANDSHAKE_BYTES: usize = 64 * 1024;
const MAX_FRAME_BYTES: usize = 1024 * 1024;
const MAX_FRAME_BYTES_U32: u32 = 1024 * 1024;
const MAX_STREAMS: u32 = 8;
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
const STREAM_TIMEOUT: Duration = Duration::from_secs(15);
const MAX_CONNECTIONS: usize = 4096;
const MAX_ENROLLMENT_CONNECTIONS: usize = 64;
const MAX_AGENT_CONNECTIONS: usize = MAX_CONNECTIONS - MAX_ENROLLMENT_CONNECTIONS;
const MAX_ATTEMPTS_PER_MINUTE: usize = 30;
const MAX_TRUST_SNAPSHOT_BINDINGS: usize = 8192;
// Per-node telemetry admission. The Agent sends one batch every 30 seconds and
// drains an offline backlog at most 32 frames per tick, each at most 512 KiB.
// The bucket therefore holds that 32-frame burst plus 8 frames of slack and
// refills at 2 frames/s and 1 MiB/s (about twice the fastest legitimate drain,
// 32 frames per 30 s), so only a misbehaving node is limited.
const TELEMETRY_BURST_EVENTS: u64 = 40;
const TELEMETRY_REFILL_EVENTS_PER_SECOND: u64 = 2;
const TELEMETRY_BURST_BYTES: u64 = 32 * 512 * 1024;
const TELEMETRY_REFILL_BYTES_PER_MILLI: u64 = 1024;
// An audit upload contains at most 32 records and the Agent waits at least
// 100 ms after each successful upload. Frame size is capped independently.
const AUDIT_BURST_BATCHES: u64 = 20;
const AUDIT_REFILL_BATCHES_PER_SECOND: u64 = 10;
// Security priority is claimed by the Agent itself and lets a frame use the
// subscriber capacity kept for control events (overflow then evicts the slow
// subscriber, which recovers by replaying retained events). Each node may claim
// it for a burst of 4 frames, refilled at one frame per minute; further claims
// are published as routine telemetry, so a forged priority cannot keep forcing
// subscribers to reconnect.
const SECURITY_TELEMETRY_FRAME_MILLIS: u64 = 60_000;
const SECURITY_TELEMETRY_BURST_MILLIS: u64 = 4 * SECURITY_TELEMETRY_FRAME_MILLIS;
/// Telemetry retained for replay never exceeds this many bytes (64 maximum
/// frames), and never more than half of the retained event slots, so
/// command, result and connection events keep their own capacity.
const MAX_RETAINED_TELEMETRY_BYTES: usize = 32 * 1024 * 1024;
/// QUIC application error code for an artifact stream aborted by this side.
const ARTIFACT_STREAM_ABORT: VarInt = VarInt::from_u32(0x108);

type EventStream = Pin<Box<dyn Stream<Item = Result<TransportEvent, Status>> + Send>>;
type ArtifactStream = Pin<Box<dyn Stream<Item = Result<ArtifactChunk, Status>> + Send>>;

#[derive(Clone)]
struct Shared {
    inner: Arc<Inner>,
}

struct Inner {
    connections: Mutex<HashMap<Vec<u8>, RegisteredConnection>>,
    registration_tokens: Mutex<HashMap<Vec<u8>, Weak<()>>>,
    fences: Mutex<HashMap<Vec<u8>, Arc<RegisteredFence>>>,
    keyring: Arc<ControllerCommandKeyring>,
    events: Mutex<EventState>,
    telemetry_budgets: Mutex<HashMap<Vec<u8>, TelemetryBudget>>,
    audit_budgets: Mutex<HashMap<Vec<u8>, AuditBudget>>,
    event_capacity: usize,
    agent_connection_permits: Arc<Semaphore>,
    enrollment_connection_permits: Arc<Semaphore>,
    shutdown: watch::Sender<bool>,
}

struct RegisteredConnection {
    metadata: NodeConnection,
    connection: Connection,
    max_message_size: usize,
    negotiated_capabilities: HashSet<String>,
    authorization_revision: u64,
    session_expires_at: Option<SystemTime>,
    fence: Arc<RegisteredFence>,
    lease_notify: Arc<Notify>,
}

/// Token bucket for one node's telemetry; tokens are milli-events and bytes.
struct TelemetryBudget {
    milli_events: u64,
    bytes: u64,
    security_millis: u64,
    updated: tokio::time::Instant,
}

struct AuditBudget {
    milli_batches: u64,
    updated: tokio::time::Instant,
}

impl Shared {
    fn new(event_capacity: usize, keyring: Arc<ControllerCommandKeyring>) -> Self {
        let capacity = event_capacity.clamp(1, MAX_CONNECTIONS);
        let (shutdown, _) = watch::channel(false);
        Self {
            inner: Arc::new(Inner {
                connections: Mutex::new(HashMap::new()),
                registration_tokens: Mutex::new(HashMap::new()),
                fences: Mutex::new(HashMap::new()),
                keyring,
                events: Mutex::new(EventState::new(capacity)),
                telemetry_budgets: Mutex::new(HashMap::new()),
                audit_budgets: Mutex::new(HashMap::new()),
                event_capacity: capacity,
                agent_connection_permits: Arc::new(Semaphore::new(MAX_AGENT_CONNECTIONS)),
                enrollment_connection_permits: Arc::new(Semaphore::new(MAX_ENROLLMENT_CONNECTIONS)),
                shutdown,
            }),
        }
    }

    /// Charges one telemetry frame to the node's bucket. `false` means the
    /// node exceeded its budget and the frame must not be published.
    async fn admit_telemetry(&self, node_id: &[u8], bytes: usize) -> bool {
        let now = tokio::time::Instant::now();
        let bytes = u64::try_from(bytes).unwrap_or(u64::MAX);
        let mut budgets = self.inner.telemetry_budgets.lock().await;
        if budgets.len() >= MAX_CONNECTIONS {
            // An idle bucket is full again, so forgetting it changes nothing.
            budgets.retain(|_, budget| now.duration_since(budget.updated) < Duration::from_mins(1));
        }
        let budget = budgets
            .entry(node_id.to_vec())
            .or_insert_with(|| TelemetryBudget {
                milli_events: TELEMETRY_BURST_EVENTS * 1000,
                bytes: TELEMETRY_BURST_BYTES,
                security_millis: SECURITY_TELEMETRY_BURST_MILLIS,
                updated: now,
            });
        let elapsed =
            u64::try_from(now.duration_since(budget.updated).as_millis()).unwrap_or(u64::MAX);
        budget.updated = now;
        budget.milli_events = budget
            .milli_events
            .saturating_add(elapsed.saturating_mul(TELEMETRY_REFILL_EVENTS_PER_SECOND))
            .min(TELEMETRY_BURST_EVENTS * 1000);
        budget.bytes = budget
            .bytes
            .saturating_add(elapsed.saturating_mul(TELEMETRY_REFILL_BYTES_PER_MILLI))
            .min(TELEMETRY_BURST_BYTES);
        budget.security_millis = budget
            .security_millis
            .saturating_add(elapsed)
            .min(SECURITY_TELEMETRY_BURST_MILLIS);
        if budget.milli_events < 1000 || budget.bytes < bytes {
            return false;
        }
        budget.milli_events -= 1000;
        budget.bytes -= bytes;
        true
    }

    /// Charges one VPN audit batch independently of the telemetry stream.
    async fn admit_audit(&self, node_id: &[u8]) -> bool {
        let now = tokio::time::Instant::now();
        let mut budgets = self.inner.audit_budgets.lock().await;
        if budgets.len() >= MAX_CONNECTIONS && !budgets.contains_key(node_id) {
            budgets.retain(|_, budget| now.duration_since(budget.updated) < Duration::from_secs(2));
            if budgets.len() >= MAX_CONNECTIONS {
                return false;
            }
        }
        let capacity = AUDIT_BURST_BATCHES * 1000;
        let budget = budgets.entry(node_id.to_vec()).or_insert(AuditBudget {
            milli_batches: capacity,
            updated: now,
        });
        let elapsed =
            u64::try_from(now.duration_since(budget.updated).as_millis()).unwrap_or(u64::MAX);
        budget.updated = now;
        budget.milli_batches = budget
            .milli_batches
            .saturating_add(elapsed.saturating_mul(AUDIT_REFILL_BATCHES_PER_SECOND))
            .min(capacity);
        if budget.milli_batches < 1000 {
            return false;
        }
        budget.milli_batches -= 1000;
        true
    }

    /// Spends the node's security-priority allowance for one admitted frame.
    /// `false` means the frame is published as routine telemetry.
    async fn claim_security_priority(&self, node_id: &[u8]) -> bool {
        let mut budgets = self.inner.telemetry_budgets.lock().await;
        let Some(budget) = budgets.get_mut(node_id) else {
            return false;
        };
        if budget.security_millis < SECURITY_TELEMETRY_FRAME_MILLIS {
            return false;
        }
        budget.security_millis -= SECURITY_TELEMETRY_FRAME_MILLIS;
        true
    }

    /// Verifies one Controller-signed operation binding against the node's
    /// registered fence term. The steps are fail-closed and ordered: both
    /// signatures and their node/endpoint pinning first, then the carrier
    /// identity, capability membership, and lease validity, and only then the
    /// exact-term match against the registered dispatch fence.
    async fn verify_operation_binding(
        &self,
        node_id: &[u8],
        endpoint_hint: Option<&[u8; 32]>,
        session_fence: Option<&RegisteredFence>,
        binding: Option<&FenceBindingV2>,
        kind: FenceOperationKind,
        operation_id: &[u8],
    ) -> Result<Option<Arc<RegisteredFence>>, Status> {
        let node_id = fixed16(node_id)?;
        let registered = self.registered_fence(&node_id).await;
        let Some(registered) = registered else {
            return Err(Status::permission_denied(
                "no owner fence is registered for this node",
            ));
        };
        let Some(binding) = binding else {
            return Err(Status::permission_denied(
                "an owner fence binding is required for this operation",
            ));
        };
        let keyring = self.keyring();
        let endpoint = endpoint_hint.copied().unwrap_or(registered.endpoint_id);
        let (now_seconds, now_nanos) = unix_now_parts();
        let claims = keyring
            .verify_fence_binding_v2_at(binding, &node_id, &endpoint, now_seconds, now_nanos)
            .map_err(|_| Status::permission_denied("fence binding is invalid"))?;
        if claims.operation_kind != kind as u32 || claims.operation_id.as_slice() != operation_id {
            return Err(Status::permission_denied(
                "fence binding does not match the requested operation",
            ));
        }
        if !registered
            .verified
            .capabilities
            .iter()
            .any(|capability| capability == &claims.capability)
        {
            return Err(Status::permission_denied(
                "fence binding capability is not held by the registered owner",
            ));
        }
        if lease_expired_at(
            registered.verified.lease_until_seconds,
            registered.verified.lease_until_nanos,
        ) {
            return Err(Status::failed_precondition(
                "the registered owner lease has expired",
            ));
        }
        if !claims.matches_fence(&registered.verified) {
            return Err(Status::permission_denied(
                "fence binding does not match the registered owner term",
            ));
        }
        match session_fence {
            None if kind == FenceOperationKind::Artifact => {
                return Err(Status::permission_denied(
                    "artifact operation requires a fenced active agent session",
                ));
            }
            Some(session_fence)
                if session_fence.endpoint_id != endpoint
                    || !same_fence_term(&session_fence.verified, &registered.verified) =>
            {
                return Err(Status::permission_denied(
                    "operation fence does not match the active agent session",
                ));
            }
            None | Some(_) => {}
        }
        Ok(Some(registered))
    }

    fn keyring(&self) -> &Arc<ControllerCommandKeyring> {
        &self.inner.keyring
    }

    async fn shutdown(&self) {
        let _ = self.inner.shutdown.send(true);
        self.inner.events.lock().await.subscribers.clear();
        let connections = {
            let mut registry = self.inner.connections.lock().await;
            registry.drain().map(|(_, value)| value).collect::<Vec<_>>()
        };
        for registered in connections {
            registered.lease_notify.notify_one();
            registered
                .connection
                .close(VarInt::from_u32(0x105), b"transport shutdown");
        }
    }
}

/// The typed UDS service backed by active Iroh connections.
#[derive(Clone)]
pub struct IrohTransportService {
    shared: Shared,
    policy: IdentityPolicy,
}

impl IrohTransportService {
    /// Creates the typed transport service with its endpoint policy and
    /// Controller fence verification keyring.
    #[must_use]
    pub fn new(
        event_capacity: usize,
        policy: IdentityPolicy,
        keyring: Arc<ControllerCommandKeyring>,
    ) -> Self {
        Self {
            shared: Shared::new(event_capacity, keyring),
            policy,
        }
    }

    /// Stops active connections and terminates all long-lived event streams.
    pub async fn begin_shutdown(&self) {
        self.shared.shutdown().await;
    }

    /// Validates one command against the node's registered session before
    /// any Agent stream is opened: negotiated capability, authorization
    /// revision, Controller grant expiry, negotiated message size, and the
    /// connection-owner fence term.
    async fn validate_command_dispatch(
        &self,
        node_id: &[u8],
        command: &CommandEnvelope,
        request_len: usize,
    ) -> Result<(Connection, usize), Status> {
        let (connection, max_message_size, session_fence) = {
            let registry = self.shared.inner.connections.lock().await;
            let entry = registry
                .get(node_id)
                .ok_or_else(|| Status::unavailable("node is not connected"))?;
            if !entry
                .negotiated_capabilities
                .contains(&command.required_capability)
            {
                return Err(Status::permission_denied(
                    "command capability was not negotiated for this session",
                ));
            }
            if entry.authorization_revision != 0
                && command.expected_revision != entry.authorization_revision
            {
                return Err(Status::failed_precondition(
                    "command authorization revision does not match the active session",
                ));
            }
            if entry
                .session_expires_at
                .is_some_and(|expires_at| expires_at <= SystemTime::now())
            {
                return Err(Status::failed_precondition(
                    "Controller session grant has expired",
                ));
            }
            if request_len > entry.max_message_size {
                return Err(Status::resource_exhausted(
                    "command exceeds the agent's negotiated message size",
                ));
            }
            (
                entry.connection.clone(),
                entry.max_message_size,
                Arc::clone(&entry.fence),
            )
        };
        self.verify_command_fence(node_id, command, &session_fence)
            .await?;
        Ok((connection, max_message_size))
    }

    /// Enforces the connection-owner fence before one command stream is
    /// opened. The predicate is fail-closed and ordered: both Controller
    /// signatures with their node/endpoint pinning first, then capability
    /// membership and carrier identity, then lease validity, and only then
    /// the exact-term match against the registered session and owner fences.
    /// A dispatch whose term predates the registered epoch is rejected, so a
    /// resumed old owner cannot reach the Agent stream.
    async fn verify_command_fence(
        &self,
        node_id: &[u8],
        command: &CommandEnvelope,
        session_fence: &RegisteredFence,
    ) -> Result<(), Status> {
        let keyring = self.shared.keyring();
        let node_id = fixed16(node_id)?;
        let (now_seconds, now_nanos) = unix_now_parts();
        let (verified, claims) = verify_command_carriers(
            keyring,
            &node_id,
            &session_fence.endpoint_id,
            command,
            now_seconds,
            now_nanos,
        )?;
        let registered = self
            .shared
            .registered_fence(&node_id)
            .await
            .ok_or_else(|| {
                Status::permission_denied("no owner fence is registered for this node")
            })?;
        if !same_fence_term(&registered.verified, &verified)
            || !same_fence_term(&session_fence.verified, &verified)
        {
            return Err(Status::permission_denied(
                "command fence does not match the registered owner term",
            ));
        }
        if !claims.matches_fence(&verified) {
            return Err(Status::permission_denied(
                "command fence binding does not match the command fence term",
            ));
        }
        Ok(())
    }
}

#[tonic::async_trait]
impl TransportService for IrohTransportService {
    type WatchEventsStream = EventStream;
    type FetchArtifactStream = ArtifactStream;

    async fn health(
        &self,
        _request: Request<HealthRequest>,
    ) -> Result<Response<HealthResponse>, Status> {
        Ok(Response::new(HealthResponse {
            status: HealthStatus::Serving.into(),
            version: env!("CARGO_PKG_VERSION").to_owned(),
        }))
    }

    async fn get_node_connection(
        &self,
        request: Request<GetNodeConnectionRequest>,
    ) -> Result<Response<NodeConnection>, Status> {
        let node_id = validate_uuid(&request.into_inner().node_id, "node_id")?;
        let metadata = self
            .shared
            .inner
            .connections
            .lock()
            .await
            .get(&node_id)
            .map(|entry| entry.metadata.clone())
            .ok_or_else(|| Status::not_found("node is not connected"))?;
        Ok(Response::new(metadata))
    }

    async fn send_command(
        &self,
        request: Request<SendCommandRequest>,
    ) -> Result<Response<SendCommandResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        if request.command_envelope.len() > MAX_FRAME_BYTES {
            return Err(Status::resource_exhausted("command exceeds 1 MiB"));
        }
        let command =
            decode_strict_command_envelope(request.command_envelope.as_slice()).map_err(|_| {
                Status::invalid_argument(
                    "command envelope protobuf is invalid or contains unknown fields",
                )
            })?;
        if command.node_id != node_id {
            return Err(Status::invalid_argument(
                "command node_id does not match request",
            ));
        }
        validate_traceparent(&command.traceparent)?;
        let response_deadline = command_response_deadline(&command)?;
        let (connection, max_message_size) = self
            .validate_command_dispatch(&node_id, &command, request.command_envelope.len())
            .await?;
        let frame = request.command_envelope;
        let response_connection = connection.clone();
        let recv = tokio::time::timeout(STREAM_TIMEOUT, async move {
            let (mut send, recv) = connection
                .open_bi()
                .await
                .map_err(|_| Status::unavailable("failed to open command stream"))?;
            let frame_len = u32::try_from(frame.len())
                .map_err(|_| Status::resource_exhausted("command length exceeds u32"))?;
            send.write_all(&frame_len.to_be_bytes())
                .await
                .map_err(|_| Status::unavailable("failed to write command length"))?;
            send.write_all(&frame)
                .await
                .map_err(|_| Status::unavailable("failed to write command"))?;
            send.finish()
                .map_err(|_| Status::unavailable("failed to finish command stream"))?;
            Ok::<_, Status>(recv)
        })
        .await
        .map_err(|_| Status::deadline_exceeded("command stream timed out"))??;
        let (dispatch_path, dispatch_path_detail, _) = metadata_path(&response_connection);
        let dispatch_path = match dispatch_path {
            ConnectionPath::Direct => "direct",
            ConnectionPath::Relay => "relay",
            ConnectionPath::Unspecified => "unspecified",
        };
        let fence = command.connection_fence.as_ref();
        tracing::info!(
            event_type = "command_frame_written",
            message_id = %hex::encode(&command.message_id),
            command_id = %hex::encode(&command.command_id),
            operation_id = %hex::encode(&command.operation_id),
            idempotency_key = %hex::encode(&command.idempotency_key),
            delivery_mode = command.delivery_mode,
            sequence = command.sequence,
            expected_revision = command.expected_revision,
            required_capability = %command.required_capability,
            semantic_payload_sha256 = %hex::encode(&command.semantic_payload_sha256),
            semantic_payload_hash_version = command.semantic_payload_hash_version,
            node_id = %hex::encode(&node_id),
            owner_fence_id = %fence.map_or_else(String::new, |value| hex::encode(&value.fence_id)),
            connection_id = %fence.map_or_else(String::new, |value| hex::encode(&value.connection_id)),
            owner_epoch = fence.map_or(0, |value| value.owner_epoch),
            path = dispatch_path,
            path_detail = %dispatch_path_detail,
            "command frame written to authenticated Agent session"
        );
        tokio::spawn(read_agent_events(
            recv,
            self.shared.clone(),
            node_id,
            max_message_size,
            response_connection,
            response_deadline,
            command,
        ));
        Ok(Response::new(SendCommandResponse { accepted: true }))
    }

    async fn fetch_artifact(
        &self,
        request: Request<FetchArtifactRequest>,
    ) -> Result<Response<Self::FetchArtifactStream>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        let artifact_id = validate_uuid(&request.artifact_id, "artifact_id")?;
        let grant = request
            .grant
            .as_ref()
            .ok_or_else(|| Status::permission_denied("artifact grant is required"))?;
        if request.purpose != "certificate_p12"
            || request.max_bytes == 0
            || request.max_bytes > 64 * 1024 * 1024
            || grant.node_id != request.node_id
            || grant.artifact_id != request.artifact_id
            || grant.purpose != request.purpose
            || grant.max_bytes != request.max_bytes
        {
            return Err(Status::invalid_argument("artifact request is invalid"));
        }
        // Acceptance boundary contract: no response headers and no artifact
        // frames may leave this handler before verify_operation_binding has
        // accepted the fence binding. The controller holds its ownership
        // guard until the first validated frame of this stream, so deferring
        // or front-loading any output past this point would reopen the
        // stale-owner fetch window the guard exists to close.
        let (connection, connection_fence) = self
            .shared
            .artifact_connection(&node_id, request.fence_binding.as_ref(), &artifact_id)
            .await?;
        let stream_deadline = artifact_stream_deadline(grant)?;
        let payload = ArtifactFetchRequest {
            artifact_id,
            purpose: request.purpose,
            max_bytes: request.max_bytes,
            grant: request.grant,
            connection_fence,
            fence_binding: request.fence_binding,
        }
        .encode_to_vec();
        let (sender, receiver) = mpsc::channel::<Result<ArtifactChunk, Status>>(8);
        tokio::spawn(relay_artifact_stream(
            connection,
            payload,
            request.artifact_id,
            request.max_bytes,
            sender,
            stream_deadline,
        ));
        Ok(Response::new(Box::pin(ReceiverStream::new(receiver))))
    }

    async fn consume_artifact(
        &self,
        request: Request<ConsumeArtifactRequest>,
    ) -> Result<Response<ConsumeArtifactResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        let grant = request
            .grant
            .as_ref()
            .ok_or_else(|| Status::permission_denied("artifact grant is required"))?;
        if grant.node_id != request.node_id
            || grant.artifact_id.len() != 16
            || grant.grant_id.len() != 16
            || grant.purpose != "certificate_p12"
            || request.sha256.len() != 32
            || request.size == 0
            || request.size != grant.max_bytes
        {
            return Err(Status::invalid_argument(
                "artifact consumption request is invalid",
            ));
        }
        let expected_artifact_id = grant.artifact_id.clone();
        let expected_grant_id = grant.grant_id.clone();
        let confirm_only = request.confirm_only;
        let (connection, connection_fence) = self
            .shared
            .artifact_connection(&node_id, request.fence_binding.as_ref(), &expected_grant_id)
            .await?;
        let payload = AgentArtifactConsumeRequest {
            grant: request.grant,
            sha256: request.sha256,
            size: request.size,
            confirm_only,
            connection_fence,
            fence_binding: request.fence_binding,
        }
        .encode_to_vec();
        let response = tokio::time::timeout(
            STREAM_TIMEOUT,
            finalize_artifact_consumption(
                connection,
                payload,
                expected_artifact_id,
                expected_grant_id,
                confirm_only,
            ),
        )
        .await
        .map_err(|_| Status::deadline_exceeded("artifact finalize stream timed out"))??;
        Ok(Response::new(ConsumeArtifactResponse {
            consumed: response.consumed,
        }))
    }

    async fn close_node(
        &self,
        request: Request<CloseNodeRequest>,
    ) -> Result<Response<CloseNodeResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        if request.reason.chars().count() > 1024 {
            return Err(Status::invalid_argument(
                "close reason exceeds 1024 characters",
            ));
        }
        self.shared
            .verify_operation_binding(
                &node_id,
                None,
                None,
                request.fence_binding.as_ref(),
                FenceOperationKind::ConnectionClose,
                &node_id,
            )
            .await
            .map(|_| ())?;
        self.shared
            .remove(&node_id, request.reason.as_bytes())
            .await
            .ok_or_else(|| Status::not_found("node is not connected"))?;
        Ok(Response::new(CloseNodeResponse {}))
    }

    async fn watch_events(
        &self,
        request: Request<WatchEventsRequest>,
    ) -> Result<Response<Self::WatchEventsStream>, Status> {
        let after = request.into_inner().after_event_id;
        if !after.is_empty() {
            validate_uuid(&after, "after_event_id")?;
        }
        let mut shutdown = self.shared.inner.shutdown.subscribe();
        if *shutdown.borrow() {
            return Err(Status::unavailable("transport is shutting down"));
        }
        let receiver = self.shared.subscribe_events(&after).await?;
        let shutdown_signal =
            async move { while !*shutdown.borrow() && shutdown.changed().await.is_ok() {} };
        Ok(Response::new(Box::pin(
            ReceiverStream::new(receiver)
                .take_until(shutdown_signal)
                .map(|item| item.map(Arc::unwrap_or_clone)),
        )))
    }

    async fn update_node_trust(
        &self,
        request: Request<UpdateNodeTrustRequest>,
    ) -> Result<Response<UpdateNodeTrustResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        if request.endpoint_id.len() != 32 {
            return Err(Status::invalid_argument("endpoint_id must be 32 bytes"));
        }
        if request.reason.is_empty() || request.reason.chars().count() > 1024 {
            return Err(Status::invalid_argument("trust update reason is invalid"));
        }
        if request.revision == 0 {
            return Err(Status::invalid_argument("trust update revision is invalid"));
        }
        let endpoint_key: [u8; 32] = request
            .endpoint_id
            .as_slice()
            .try_into()
            .map_err(|_| Status::invalid_argument("endpoint_id is invalid"))?;
        let endpoint = EndpointId::from_bytes(&endpoint_key)
            .map_err(|_| Status::invalid_argument("endpoint_id is invalid"))?;
        let state = NodeTrustState::try_from(request.state)
            .map_err(|_| Status::invalid_argument("trust state is invalid"))?;
        if state == NodeTrustState::Unspecified {
            return Err(Status::invalid_argument("trust state is unspecified"));
        }
        // The operation identity is derived from the exact carrier, so the
        // binding must be replayed with the identical endpoint, state,
        // revision, and reason to pass; the declared identity must also match
        // the derivation, which makes the carrier explicit.
        let node_key = fixed16(&node_id)?;
        let derived_operation_id = ocservia_command_authorization::state_update_operation_id(
            &node_key,
            &endpoint_key,
            u32::try_from(request.state)
                .map_err(|_| Status::invalid_argument("trust state is invalid"))?,
            request.revision,
            &request.reason,
        );
        if !request.operation_id.is_empty()
            && request.operation_id.as_slice() != derived_operation_id.as_slice()
        {
            return Err(Status::invalid_argument(
                "trust update operation identity does not match its carrier",
            ));
        }
        self.shared
            .verify_operation_binding(
                &node_id,
                Some(&endpoint_key),
                None,
                request.fence_binding.as_ref(),
                FenceOperationKind::StateUpdate,
                &request.operation_id,
            )
            .await
            .map(|_| ())?;
        let (disposition, previous_revision, retained_revision, retained_state) = self
            .policy
            .update_with_status(endpoint, node_id.clone(), state, request.revision);
        if disposition == PolicyUpdateDisposition::Applied
            && retained_revision == request.revision
            && retained_state == state
            && retained_revision > previous_revision
        {
            self.shared
                .retire_prior_authorization(&node_id, retained_revision, state)
                .await;
        }
        let disposition = match disposition {
            PolicyUpdateDisposition::Applied => TrustUpdateDisposition::Applied,
            PolicyUpdateDisposition::Stale => TrustUpdateDisposition::Stale,
            PolicyUpdateDisposition::Rejected => TrustUpdateDisposition::Rejected,
        };
        Ok(Response::new(UpdateNodeTrustResponse {
            disposition: disposition.into(),
            retained_revision,
            retained_state: retained_state.into(),
        }))
    }

    async fn register_owner_fence(
        &self,
        request: Request<RegisterOwnerFenceRequest>,
    ) -> Result<Response<RegisterOwnerFenceResponse>, Status> {
        let request = request.into_inner();
        let fence = request
            .fence
            .ok_or_else(|| Status::invalid_argument("owner fence is required"))?;
        self.shared.register_fence(fence).await.map(Response::new)
    }

    async fn get_owner_fence(
        &self,
        request: Request<GetOwnerFenceRequest>,
    ) -> Result<Response<GetOwnerFenceResponse>, Status> {
        let node_id = validate_uuid(&request.into_inner().node_id, "node_id")?;
        let fence = self
            .shared
            .registered_fence(&fixed16(&node_id)?)
            .await
            .map(|registered| registered.fence.clone())
            .ok_or_else(|| Status::not_found("owner fence is not registered"))?;
        Ok(Response::new(GetOwnerFenceResponse { fence: Some(fence) }))
    }
}

/// Builds the controller endpoint with a live Go trust authority and
/// additional relay TLS roots for privately certificated relays.
///
/// # Errors
///
/// Returns an Iroh bind error when endpoint or transport initialization fails.
pub async fn build_router_with_trust_tls_roots(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    trust: TrustAuthority,
    relay_tls_roots: Vec<rustls_pki_types::CertificateDer<'static>>,
    service: &IrohTransportService,
) -> Result<Router, AcceptError> {
    trust.load_snapshot(&policy).await?;
    build_router_with_direct(
        secret_key,
        relay_mode,
        policy,
        trust,
        service,
        true,
        relay_tls_roots,
    )
    .await
    .map_err(|_| protocol_error("iroh transport bind failed"))
}

async fn build_router_with_direct(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    trust: TrustAuthority,
    service: &IrohTransportService,
    direct_enabled: bool,
    relay_tls_roots: Vec<rustls_pki_types::CertificateDer<'static>>,
) -> Result<Router, iroh::endpoint::BindError> {
    let transport = QuicTransportConfig::builder()
        .max_concurrent_bidi_streams(VarInt::from_u32(MAX_STREAMS))
        .max_concurrent_uni_streams(VarInt::from_u32(2))
        .max_idle_timeout(Some(VarInt::from_u32(90_000).into()))
        .stream_receive_window(VarInt::from_u32(MAX_FRAME_BYTES_U32))
        .receive_window(VarInt::from_u32(MAX_FRAME_BYTES_U32 * 4))
        .datagram_receive_buffer_size(None)
        .build();
    let mut endpoint_builder = Endpoint::builder(presets::N0)
        .secret_key(secret_key)
        .relay_mode(relay_mode)
        .clear_address_lookup()
        // System DNS and hosts only: fail visibly instead of querying iroh's
        // public fallback resolvers.
        .dns_resolver(
            iroh::dns::DnsResolver::builder()
                .with_system_defaults()
                .disable_fallback()
                .build(),
        )
        .keep_relays_connected(false)
        .transport_config(transport)
        .hooks(SecurityHook::new(policy.clone(), &trust));
    if !relay_tls_roots.is_empty() {
        endpoint_builder = endpoint_builder
            .ca_tls_config(iroh::tls::CaTlsConfig::default().with_extra_roots(relay_tls_roots));
    }
    if !direct_enabled {
        endpoint_builder = endpoint_builder.clear_ip_transports();
    }
    let endpoint = endpoint_builder.bind().await?;
    Ok(Router::builder(endpoint)
        .accept(
            ENROLL_ALPN,
            SessionHandler {
                shared: service.shared.clone(),
                policy: policy.clone(),
                kind: ProtocolKind::Enroll,
                trust: trust.clone(),
                handshake_timeout: HANDSHAKE_TIMEOUT,
            },
        )
        .accept(
            AGENT_ALPN,
            SessionHandler {
                shared: service.shared.clone(),
                policy,
                kind: ProtocolKind::Agent,
                trust,
                handshake_timeout: HANDSHAKE_TIMEOUT,
            },
        )
        .spawn())
}

/// Shuts down active connections before closing the Iroh router.
///
/// # Errors
///
/// Returns an error when the Iroh router task cannot finish shutting down.
pub async fn shutdown(
    service: &IrohTransportService,
    router: Router,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    service.begin_shutdown().await;
    router.shutdown().await.map_err(Into::into)
}

#[cfg(test)]
mod tests;
