//! Iroh endpoint ownership, protocol routing, and the typed UDS transport service.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet};
use std::pin::Pin;
use std::sync::{Arc, Weak};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

mod admission;
mod artifacts;
mod command_stream;
mod events;
mod monitors;
mod registry;
mod trust;

use admission::SecurityHook;
#[cfg(test)]
use admission::{AttemptRejection, record_attempt, record_global_attempt};
use artifacts::{artifact_stream_deadline, fetch_artifact_payload, finalize_artifact_consumption};
use command_stream::{command_response_deadline, read_agent_events, validate_traceparent};
#[cfg(test)]
use command_stream::{event, new_traceparent, now_timestamp, privileged_result_proof_error};
use events::EventState;
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
    AgentEvent, AgentEventType, CommandResult, CommandResultState, EnrollRequest, EnrollResponse,
    HandshakeResult, SessionHandshake, SessionHandshakeResponse, TelemetryBatch, command_envelope,
};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactConsumeRequest as AgentArtifactConsumeRequest,
    ArtifactConsumeResponse as AgentArtifactConsumeResponse, ArtifactFetchRequest, ArtifactGrantV1,
    ArtifactPayload, CommandEnvelope,
};
#[cfg(test)]
use ocservia_contracts::generated::ocserv::platform::transport::v1::TransportEventType;
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    CloseNodeRequest, CloseNodeResponse, ConnectionPath, ConsumeArtifactRequest,
    ConsumeArtifactResponse, FetchArtifactRequest, GetNodeConnectionRequest, HealthRequest,
    HealthResponse, HealthStatus, NodeConnection, NodeTrustState, SendCommandRequest,
    SendCommandResponse, TransportEvent, TrustUpdateDisposition, UpdateNodeTrustRequest,
    UpdateNodeTrustResponse, WatchEventsRequest, transport_service_server::TransportService,
};
use ocservia_contracts::session::{ARTIFACT_CONSUME_FRAME, ARTIFACT_FETCH_FRAME};
use prost::Message;
use tokio::sync::{Mutex, Semaphore, watch};
use tokio_stream::{Stream, wrappers::ReceiverStream};
use tonic::{Request, Response, Status};
use trust::PolicyUpdateDisposition;
#[cfg(test)]
use trust::TrustBudgetSet;
pub use trust::{
    IdentityPolicy, TrustAuthority, TrustBudgetConfig, TrustClass, TrustClassLimit,
    TrustMetricsSnapshot,
};
use uuid::Uuid;

#[cfg(test)]
use std::collections::VecDeque;
#[cfg(test)]
use std::sync::atomic::{AtomicU64, Ordering};
#[cfg(test)]
use std::time::Instant;

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
const TELEMETRY_BURST_EVENTS: u64 = 40;
const TELEMETRY_REFILL_EVENTS_PER_SECOND: u64 = 2;
const TELEMETRY_BURST_BYTES: u64 = 32 * 512 * 1024;
const TELEMETRY_REFILL_BYTES_PER_MILLI: u64 = 1024;
// An audit upload contains at most 32 records and the Agent waits at least
// 100 ms after each successful upload. Frame size is capped independently.
const AUDIT_BURST_BATCHES: u64 = 20;
const AUDIT_REFILL_BATCHES_PER_SECOND: u64 = 10;
const SECURITY_TELEMETRY_FRAME_MILLIS: u64 = 60_000;
const SECURITY_TELEMETRY_BURST_MILLIS: u64 = 4 * SECURITY_TELEMETRY_FRAME_MILLIS;
const MAX_RETAINED_TELEMETRY_BYTES: usize = 32 * 1024 * 1024;
const ARTIFACT_STREAM_ABORT: VarInt = VarInt::from_u32(0x108);

type EventStream = Pin<Box<dyn Stream<Item = Result<TransportEvent, Status>> + Send>>;

pub(crate) fn validate_uuid(value: &[u8], field: &str) -> Result<Vec<u8>, Status> {
    let id = Uuid::from_slice(value)
        .map_err(|_| Status::invalid_argument(format!("{field} must be a 16-byte UUIDv7")))?;
    if id.get_version_num() != 7 {
        return Err(Status::invalid_argument(format!(
            "{field} must be a UUIDv7"
        )));
    }
    Ok(value.to_vec())
}

pub(crate) fn unix_now_seconds() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|duration| i64::try_from(duration.as_secs()).ok())
        .unwrap_or(0)
}

#[derive(Clone)]
struct Shared {
    inner: Arc<Inner>,
}

struct Inner {
    connections: Mutex<HashMap<Vec<u8>, RegisteredConnection>>,
    registration_tokens: Mutex<HashMap<Vec<u8>, Weak<()>>>,
    session_epochs: Mutex<HashMap<Vec<u8>, u64>>,
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
    session_epoch: u64,
    connection_id: [u8; 16],
    session_expires_at: SystemTime,
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
                session_epochs: Mutex::new(HashMap::new()),
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
    /// Controller verification keyring.
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
    /// revision, Controller grant expiry, and negotiated message size.
    async fn validate_command_dispatch(
        &self,
        node_id: &[u8],
        command: &CommandEnvelope,
        request_len: usize,
    ) -> Result<(Connection, usize, [u8; 16], u64), Status> {
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
        if entry.session_expires_at <= SystemTime::now() {
            return Err(Status::failed_precondition(
                "Controller session grant has expired",
            ));
        }
        if request_len > entry.max_message_size {
            return Err(Status::resource_exhausted(
                "command exceeds the agent's negotiated message size",
            ));
        }
        Ok((
            entry.connection.clone(),
            entry.max_message_size,
            entry.connection_id,
            entry.session_epoch,
        ))
    }
}

#[tonic::async_trait]
impl TransportService for IrohTransportService {
    type WatchEventsStream = EventStream;

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
        let (connection, max_message_size, connection_id, session_epoch) = self
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
            connection_id = %hex::encode(connection_id),
            session_epoch,
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
    ) -> Result<Response<ArtifactPayload>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        let artifact_id = validate_uuid(&request.artifact_id, "artifact_id")?;
        let grant = request
            .grant
            .as_ref()
            .ok_or_else(|| Status::permission_denied("artifact grant is required"))?;
        if request.purpose != "certificate_p12"
            || request.max_bytes == 0
            || request.max_bytes > 256 * 1024
            || grant.node_id != request.node_id
            || grant.artifact_id != request.artifact_id
            || grant.purpose != request.purpose
            || grant.max_bytes != request.max_bytes
        {
            return Err(Status::invalid_argument("artifact request is invalid"));
        }
        let connection = self.shared.artifact_connection(&node_id).await?;
        let deadline = artifact_stream_deadline(grant)?;
        let payload = ArtifactFetchRequest {
            artifact_id: artifact_id.clone(),
            purpose: request.purpose,
            max_bytes: request.max_bytes,
            grant: request.grant,
        }
        .encode_to_vec();
        let artifact = fetch_artifact_payload(
            connection,
            payload,
            &artifact_id,
            request.max_bytes,
            deadline,
        )
        .await?;
        Ok(Response::new(artifact))
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
        let connection = self.shared.artifact_connection(&node_id).await?;
        let payload = AgentArtifactConsumeRequest {
            grant: request.grant,
            sha256: request.sha256,
            size: request.size,
            confirm_only,
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
        .dns_resolver(
            iroh::dns::DnsResolver::builder()
                .with_system_defaults()
                .disable_fallback()
                .build(),
        )
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
