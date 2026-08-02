//! Iroh endpoint ownership, protocol routing, and the typed UDS transport service.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet, VecDeque};
use std::pin::Pin;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime};

use futures_util::StreamExt as _;
use iroh::endpoint::{
    AfterHandshakeOutcome, Connection, EndpointHooks, PathEvent, QuicTransportConfig, RelayMode,
    VarInt, presets,
};
use iroh::protocol::{AcceptError, ProtocolHandler, Router};
use iroh::{Endpoint, EndpointId, SecretKey};
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    AgentEvent, AgentEventType, CommandEnvelope, HandshakeResult, SessionHandshake,
    SessionHandshakeResponse,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    CloseNodeRequest, CloseNodeResponse, ConnectionPath, GetNodeConnectionRequest, HealthRequest,
    HealthResponse, HealthStatus, NodeConnection, SendCommandRequest, SendCommandResponse,
    TransportEvent, TransportEventType, WatchEventsRequest,
    transport_service_server::TransportService,
};
use prost::Message;
use tokio::sync::{Mutex, Semaphore, mpsc, watch};
use tokio_stream::{Stream, wrappers::ReceiverStream};
use tonic::{Request, Response, Status};
use uuid::Uuid;

/// ALPN for one-time enrollment connections.
pub const ENROLL_ALPN: &[u8] = b"ocserv-platform/enroll/1";
/// ALPN for approved agent sessions.
pub const AGENT_ALPN: &[u8] = b"ocserv-platform/agent/1";

const PROTOCOL_MAJOR: u32 = 1;
const PROTOCOL_MINOR: u32 = 0;
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
const MAX_RESPONSE_FRAMES: usize = 128;

type EventStream = Pin<Box<dyn Stream<Item = Result<TransportEvent, Status>> + Send>>;

/// A bounded, transport-only identity policy supplied at process startup.
#[derive(Clone, Debug, Default)]
pub struct IdentityPolicy {
    approved: Arc<HashMap<EndpointId, Vec<u8>>>,
    revoked: Arc<HashSet<EndpointId>>,
}

impl IdentityPolicy {
    /// Builds a policy from endpoint-to-node bindings and revoked identifiers.
    #[must_use]
    pub fn new(approved: HashMap<EndpointId, Vec<u8>>, revoked: HashSet<EndpointId>) -> Self {
        Self {
            approved: Arc::new(approved),
            revoked: Arc::new(revoked),
        }
    }

    fn permits(&self, endpoint: EndpointId, alpn: &[u8]) -> bool {
        if self.revoked.contains(&endpoint) {
            return false;
        }
        alpn == ENROLL_ALPN || (alpn == AGENT_ALPN && self.approved.contains_key(&endpoint))
    }

    fn matches_node(&self, endpoint: EndpointId, node_id: &[u8]) -> bool {
        self.approved
            .get(&endpoint)
            .is_some_and(|approved_node| approved_node == node_id)
    }
}

#[derive(Debug)]
struct SecurityHook {
    policy: IdentityPolicy,
    attempts: Mutex<HashMap<EndpointId, VecDeque<Instant>>>,
}

impl SecurityHook {
    fn new(policy: IdentityPolicy) -> Self {
        Self {
            policy,
            attempts: Mutex::new(HashMap::new()),
        }
    }
}

impl EndpointHooks for SecurityHook {
    async fn after_handshake(&self, connection: &Connection) -> AfterHandshakeOutcome {
        let endpoint = connection.remote_id();
        let alpn = connection.alpn();
        if alpn != ENROLL_ALPN && alpn != AGENT_ALPN {
            return reject(0x100, b"unsupported protocol");
        }
        if !self.policy.permits(endpoint, alpn) {
            return reject(0x101, b"endpoint not permitted");
        }

        let now = Instant::now();
        let mut attempts = self.attempts.lock().await;
        attempts.retain(|_, endpoint_attempts| {
            while endpoint_attempts
                .front()
                .is_some_and(|at| now.duration_since(*at) >= Duration::from_mins(1))
            {
                endpoint_attempts.pop_front();
            }
            !endpoint_attempts.is_empty()
        });
        if !attempts.contains_key(&endpoint) && attempts.len() >= MAX_CONNECTIONS {
            return reject(0x102, b"connection rate capacity reached");
        }
        let endpoint_attempts = attempts.entry(endpoint).or_default();
        if endpoint_attempts.len() >= MAX_ATTEMPTS_PER_MINUTE {
            return reject(0x103, b"connection rate exceeded");
        }
        endpoint_attempts.push_back(now);
        AfterHandshakeOutcome::Accept
    }
}

fn reject(code: u32, reason: &[u8]) -> AfterHandshakeOutcome {
    AfterHandshakeOutcome::Reject {
        error_code: VarInt::from_u32(code),
        reason: reason.to_vec(),
    }
}

#[derive(Clone)]
struct Shared {
    inner: Arc<Inner>,
}

struct Inner {
    connections: Mutex<HashMap<Vec<u8>, RegisteredConnection>>,
    events: Mutex<EventState>,
    event_capacity: usize,
    agent_connection_permits: Arc<Semaphore>,
    enrollment_connection_permits: Arc<Semaphore>,
    shutdown: watch::Sender<bool>,
}

struct RegisteredConnection {
    metadata: NodeConnection,
    connection: Connection,
    max_message_size: usize,
}

struct EventState {
    retained: VecDeque<TransportEvent>,
    subscribers: Vec<mpsc::Sender<Result<TransportEvent, Status>>>,
}

impl Shared {
    fn new(event_capacity: usize) -> Self {
        let capacity = event_capacity.clamp(1, MAX_CONNECTIONS);
        let (shutdown, _) = watch::channel(false);
        Self {
            inner: Arc::new(Inner {
                connections: Mutex::new(HashMap::new()),
                events: Mutex::new(EventState {
                    retained: VecDeque::with_capacity(capacity),
                    subscribers: Vec::new(),
                }),
                event_capacity: capacity,
                agent_connection_permits: Arc::new(Semaphore::new(MAX_AGENT_CONNECTIONS)),
                enrollment_connection_permits: Arc::new(Semaphore::new(MAX_ENROLLMENT_CONNECTIONS)),
                shutdown,
            }),
        }
    }

    async fn publish(&self, event: TransportEvent) {
        if *self.inner.shutdown.borrow() {
            return;
        }
        let mut state = self.inner.events.lock().await;
        if state.retained.len() == self.inner.event_capacity {
            state.retained.pop_front();
        }
        state.retained.push_back(event.clone());
        state
            .subscribers
            .retain(|sender| matches!(sender.try_send(Ok(event.clone())), Ok(())));
    }

    async fn remove(&self, node_id: &[u8], reason: &[u8]) -> Option<RegisteredConnection> {
        let registered = self.inner.connections.lock().await.remove(node_id);
        if let Some(registered) = &registered {
            registered.connection.close(VarInt::from_u32(0x104), reason);
            self.publish(event(
                node_id,
                TransportEventType::Disconnected,
                reason.to_vec(),
            ))
            .await;
        }
        registered
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
}

impl IrohTransportService {
    /// Creates the service and its bounded connection registry.
    #[must_use]
    pub fn new(event_capacity: usize) -> Self {
        Self {
            shared: Shared::new(event_capacity),
        }
    }

    /// Stops active connections and terminates all long-lived event streams.
    pub async fn begin_shutdown(&self) {
        self.shared.shutdown().await;
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
        let command = CommandEnvelope::decode(request.command_envelope.as_slice())
            .map_err(|_| Status::invalid_argument("command envelope protobuf is invalid"))?;
        if command.node_id != node_id {
            return Err(Status::invalid_argument(
                "command node_id does not match request",
            ));
        }
        validate_traceparent(&command.traceparent)?;
        let (connection, max_message_size) = self
            .shared
            .inner
            .connections
            .lock()
            .await
            .get_mut(&node_id)
            .map(|entry| (entry.connection.clone(), entry.max_message_size))
            .ok_or_else(|| Status::unavailable("node is not connected"))?;
        if request.command_envelope.len() > max_message_size {
            return Err(Status::resource_exhausted(
                "command exceeds the agent's negotiated message size",
            ));
        }
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
        tokio::spawn(read_agent_events(
            recv,
            self.shared.clone(),
            node_id,
            command.traceparent,
            max_message_size,
            response_connection,
        ));
        Ok(Response::new(SendCommandResponse { accepted: true }))
    }

    async fn close_node(
        &self,
        request: Request<CloseNodeRequest>,
    ) -> Result<Response<CloseNodeResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_uuid(&request.node_id, "node_id")?;
        if request.reason.len() > 1024 {
            return Err(Status::invalid_argument("close reason exceeds 1 KiB"));
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
        let mut state = self.shared.inner.events.lock().await;
        state.subscribers.retain(|sender| !sender.is_closed());
        if !after.is_empty() && !state.retained.iter().any(|item| item.event_id == after) {
            return Err(Status::out_of_range("event cursor is outside retention"));
        }
        if state.subscribers.len() == self.shared.inner.event_capacity {
            return Err(Status::resource_exhausted(
                "event subscriber capacity reached",
            ));
        }
        let backlog = state
            .retained
            .iter()
            .skip_while(|item| !after.is_empty() && item.event_id != after)
            .skip(usize::from(!after.is_empty()))
            .cloned()
            .collect::<Vec<_>>();
        let (sender, receiver) = mpsc::channel(self.shared.inner.event_capacity);
        for item in backlog {
            sender
                .try_send(Ok(item))
                .map_err(|_| Status::internal("retained event queue overflow"))?;
        }
        state.subscribers.push(sender);
        drop(state);
        let shutdown_signal =
            async move { while !*shutdown.borrow() && shutdown.changed().await.is_ok() {} };
        Ok(Response::new(Box::pin(
            ReceiverStream::new(receiver).take_until(shutdown_signal),
        )))
    }
}

#[derive(Clone, Copy, Debug)]
enum ProtocolKind {
    Enroll,
    Agent,
}

#[derive(Clone)]
struct SessionHandler {
    shared: Shared,
    policy: IdentityPolicy,
    kind: ProtocolKind,
}

impl std::fmt::Debug for SessionHandler {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("SessionHandler")
            .field("kind", &self.kind)
            .finish_non_exhaustive()
    }
}

impl ProtocolHandler for SessionHandler {
    async fn accept(&self, connection: Connection) -> Result<(), AcceptError> {
        let permits = match self.kind {
            ProtocolKind::Enroll => &self.shared.inner.enrollment_connection_permits,
            ProtocolKind::Agent => &self.shared.inner.agent_connection_permits,
        };
        let permit = permits
            .clone()
            .try_acquire_owned()
            .map_err(|_| protocol_error("connection capacity reached"))?;
        let (mut send, mut recv) = tokio::time::timeout(HANDSHAKE_TIMEOUT, connection.accept_bi())
            .await
            .map_err(|_| protocol_error("handshake stream timed out"))?
            .map_err(|_| protocol_error("handshake stream failed"))?;
        let handshake = read_handshake(&mut recv).await?;
        validate_handshake(&handshake, connection.remote_id())?;
        if matches!(self.kind, ProtocolKind::Agent)
            && !self
                .policy
                .matches_node(connection.remote_id(), &handshake.node_id)
        {
            return Err(protocol_error("endpoint is not bound to the claimed node"));
        }

        let result = match self.kind {
            ProtocolKind::Enroll => HandshakeResult::PendingApproval,
            ProtocolKind::Agent => HandshakeResult::Accepted,
        };
        write_handshake_response(&mut send, result, handshake.max_message_size).await?;
        if matches!(self.kind, ProtocolKind::Enroll) {
            connection.close(VarInt::from_u32(0), b"enrollment pending");
            return Ok(());
        }

        connection.set_max_concurrent_bi_streams(VarInt::from_u32(MAX_STREAMS));
        connection.set_max_concurrent_uni_streams(VarInt::from_u32(2));
        let metadata = metadata(&handshake, &connection).await;
        let node_id = metadata.node_id.clone();
        let replaced = self.shared.inner.connections.lock().await.insert(
            node_id.clone(),
            RegisteredConnection {
                metadata: metadata.clone(),
                connection: connection.clone(),
                max_message_size: handshake.max_message_size as usize,
            },
        );
        if let Some(previous) = replaced {
            previous
                .connection
                .close(VarInt::from_u32(0x106), b"superseded connection");
        }
        self.shared
            .publish(event(
                &node_id,
                TransportEventType::Connected,
                metadata.path_detail.as_bytes().to_vec(),
            ))
            .await;

        let monitor = monitor_paths(self.shared.clone(), node_id.clone(), connection.clone());
        let _closed = connection.closed().await;
        monitor.abort();
        let mut registry = self.shared.inner.connections.lock().await;
        if registry
            .get(&node_id)
            .is_some_and(|entry| entry.connection.stable_id() == connection.stable_id())
        {
            registry.remove(&node_id);
            drop(registry);
            self.shared
                .publish(event(
                    &node_id,
                    TransportEventType::Disconnected,
                    b"connection closed".to_vec(),
                ))
                .await;
        }
        drop(permit);
        Ok(())
    }
}

fn monitor_paths(
    shared: Shared,
    node_id: Vec<u8>,
    connection: Connection,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut events = connection.path_events();
        while let Some(path_event) = events.next().await {
            if !matches!(
                path_event,
                PathEvent::Selected { .. } | PathEvent::Lagged { .. }
            ) {
                continue;
            }
            let next = metadata_path(&connection);
            let mut registry = shared.inner.connections.lock().await;
            let Some(entry) = registry.get_mut(&node_id) else {
                break;
            };
            if entry.connection.stable_id() != connection.stable_id() {
                break;
            }
            entry.metadata.path = next.0.into();
            entry.metadata.path_detail.clone_from(&next.1);
            entry.metadata.round_trip_time_millis = next.2;
            entry.metadata.last_seen = Some(now_timestamp());
            drop(registry);
            shared
                .publish(event(
                    &node_id,
                    TransportEventType::PathChanged,
                    next.1.into_bytes(),
                ))
                .await;
        }
    })
}

async fn read_agent_events(
    mut recv: iroh::endpoint::RecvStream,
    shared: Shared,
    node_id: Vec<u8>,
    traceparent: String,
    max_message_size: usize,
    connection: Connection,
) {
    for _ in 0..MAX_RESPONSE_FRAMES {
        let mut length = [0_u8; 4];
        match tokio::time::timeout(STREAM_TIMEOUT, recv.read_exact(&mut length)).await {
            Ok(Ok(())) => {}
            Ok(Err(_)) => {
                publish_agent_error(
                    &shared,
                    &node_id,
                    &traceparent,
                    "agent response ended before a terminal event",
                )
                .await;
                return;
            }
            Err(_) => {
                publish_agent_error(&shared, &node_id, &traceparent, "agent response timed out")
                    .await;
                return;
            }
        }
        let length = u32::from_be_bytes(length) as usize;
        if length == 0 || length > max_message_size || length > MAX_FRAME_BYTES {
            publish_agent_error(
                &shared,
                &node_id,
                &traceparent,
                "agent response size invalid",
            )
            .await;
            return;
        }
        let mut bytes = vec![0_u8; length];
        if !matches!(
            tokio::time::timeout(STREAM_TIMEOUT, recv.read_exact(&mut bytes)).await,
            Ok(Ok(()))
        ) {
            publish_agent_error(
                &shared,
                &node_id,
                &traceparent,
                "agent response body invalid",
            )
            .await;
            return;
        }
        let Ok(agent_event) = AgentEvent::decode(bytes.as_slice()) else {
            publish_agent_error(
                &shared,
                &node_id,
                &traceparent,
                "agent response protobuf invalid",
            )
            .await;
            return;
        };
        let event_type = match AgentEventType::try_from(agent_event.r#type) {
            Ok(AgentEventType::CommandResult) => TransportEventType::CommandResult,
            Ok(AgentEventType::Heartbeat) => TransportEventType::Heartbeat,
            Ok(AgentEventType::Error) => TransportEventType::Error,
            _ => {
                publish_agent_error(
                    &shared,
                    &node_id,
                    &traceparent,
                    "agent response type invalid",
                )
                .await;
                return;
            }
        };
        let terminal = matches!(
            event_type,
            TransportEventType::CommandResult | TransportEventType::Error
        );
        touch_connection(&shared, &node_id, &connection).await;
        shared
            .publish(event_with_traceparent(
                &node_id,
                event_type,
                agent_event.payload,
                &traceparent,
            ))
            .await;
        if terminal {
            return;
        }
    }
    publish_agent_error(
        &shared,
        &node_id,
        &traceparent,
        "agent response frame limit exceeded",
    )
    .await;
}

async fn publish_agent_error(shared: &Shared, node_id: &[u8], traceparent: &str, message: &str) {
    shared
        .publish(event_with_traceparent(
            node_id,
            TransportEventType::Error,
            message.as_bytes().to_vec(),
            traceparent,
        ))
        .await;
}

async fn touch_connection(shared: &Shared, node_id: &[u8], connection: &Connection) {
    let mut registry = shared.inner.connections.lock().await;
    if let Some(entry) = registry
        .get_mut(node_id)
        .filter(|entry| entry.connection.stable_id() == connection.stable_id())
    {
        entry.metadata.last_seen = Some(now_timestamp());
    }
}

async fn read_handshake(
    recv: &mut iroh::endpoint::RecvStream,
) -> Result<SessionHandshake, AcceptError> {
    let mut length = [0_u8; 4];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut length))
        .await
        .map_err(|_| protocol_error("handshake length timed out"))?
        .map_err(|_| protocol_error("handshake length invalid"))?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > MAX_HANDSHAKE_BYTES {
        return Err(protocol_error("handshake size invalid"));
    }
    let mut bytes = vec![0_u8; length];
    tokio::time::timeout(HANDSHAKE_TIMEOUT, recv.read_exact(&mut bytes))
        .await
        .map_err(|_| protocol_error("handshake body timed out"))?
        .map_err(|_| protocol_error("handshake body invalid"))?;
    SessionHandshake::decode(bytes.as_slice())
        .map_err(|_| protocol_error("handshake protobuf invalid"))
}

async fn write_handshake_response(
    send: &mut iroh::endpoint::SendStream,
    result: HandshakeResult,
    max_message_size: u32,
) -> Result<(), AcceptError> {
    let response = SessionHandshakeResponse {
        result: result.into(),
        protocol_major: PROTOCOL_MAJOR,
        protocol_minor: PROTOCOL_MINOR,
        max_message_size,
        controller_version: env!("CARGO_PKG_VERSION").to_owned(),
    }
    .encode_to_vec();
    let response_len = u32::try_from(response.len())
        .map_err(|_| protocol_error("handshake response length exceeds u32"))?;
    send.write_all(&response_len.to_be_bytes())
        .await
        .map_err(|_| protocol_error("handshake response length failed"))?;
    send.write_all(&response)
        .await
        .map_err(|_| protocol_error("handshake response failed"))?;
    send.finish()
        .map_err(|_| protocol_error("handshake response finish failed"))?;
    Ok(())
}

fn validate_handshake(handshake: &SessionHandshake, remote: EndpointId) -> Result<(), AcceptError> {
    validate_uuid(&handshake.node_id, "node_id").map_err(|status| status_to_accept(&status))?;
    validate_uuid(&handshake.agent_instance_id, "agent_instance_id")
        .map_err(|status| status_to_accept(&status))?;
    if handshake.endpoint_id != remote.as_bytes() {
        return Err(protocol_error("endpoint identity mismatch"));
    }
    if handshake.protocol_major != PROTOCOL_MAJOR || handshake.protocol_minor > PROTOCOL_MINOR {
        return Err(protocol_error("protocol version incompatible"));
    }
    if handshake.max_message_size == 0 || handshake.max_message_size as usize > MAX_FRAME_BYTES {
        return Err(protocol_error("message size incompatible"));
    }
    if handshake.nonce.len() < 16 || handshake.nonce.len() > 64 {
        return Err(protocol_error("nonce size invalid"));
    }
    if handshake.capabilities.len() > 128
        || handshake.supported_compressions.len() > 16
        || [
            &handshake.agent_version,
            &handshake.controller_version,
            &handshake.ocserv_version,
            &handshake.os_release,
            &handshake.boot_id,
        ]
        .iter()
        .any(|value| value.len() > 256)
    {
        return Err(protocol_error("handshake field limit exceeded"));
    }
    Ok(())
}

fn status_to_accept(status: &Status) -> AcceptError {
    protocol_error(status.message())
}

fn protocol_error(message: &str) -> AcceptError {
    AcceptError::from_err(std::io::Error::new(
        std::io::ErrorKind::InvalidData,
        message.to_owned(),
    ))
}

async fn metadata(handshake: &SessionHandshake, connection: &Connection) -> NodeConnection {
    let path_metadata = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let next = metadata_path(connection);
            if next.0 != ConnectionPath::Unspecified {
                break next;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap_or_else(|_| metadata_path(connection));
    let (path, detail, rtt) = path_metadata;
    let now = now_timestamp();
    NodeConnection {
        node_id: handshake.node_id.clone(),
        endpoint_id: connection.remote_id().as_bytes().to_vec(),
        path: path.into(),
        round_trip_time_millis: rtt,
        connected_at: Some(now),
        agent_instance_id: handshake.agent_instance_id.clone(),
        path_detail: detail,
        last_seen: Some(now),
    }
}

fn metadata_path(connection: &Connection) -> (ConnectionPath, String, u64) {
    let paths = connection.paths();
    let selected = paths
        .iter()
        .find(iroh::endpoint::Path::is_selected)
        .or_else(|| paths.iter().next());
    selected.map_or(
        (ConnectionPath::Unspecified, "unknown".to_owned(), 0),
        |path| {
            let kind = if path.is_ip() {
                ConnectionPath::Direct
            } else if path.is_relay() {
                ConnectionPath::Relay
            } else {
                ConnectionPath::Unspecified
            };
            let detail = format!("{:?}", path.remote_addr());
            let millis = u64::try_from(path.rtt().as_millis()).unwrap_or(u64::MAX);
            (kind, detail, millis)
        },
    )
}

fn validate_uuid(value: &[u8], name: &'static str) -> Result<Vec<u8>, Status> {
    let id = Uuid::from_slice(value)
        .map_err(|_| Status::invalid_argument(format!("{name} must be a 16-byte UUID")))?;
    if id.get_version_num() != 7 {
        return Err(Status::invalid_argument(format!("{name} must be UUIDv7")));
    }
    Ok(value.to_vec())
}

fn event(node_id: &[u8], kind: TransportEventType, payload: Vec<u8>) -> TransportEvent {
    event_with_traceparent(node_id, kind, payload, &new_traceparent())
}

fn event_with_traceparent(
    node_id: &[u8],
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
    }
}

fn validate_traceparent(value: &str) -> Result<(), Status> {
    let parts = value.split('-').collect::<Vec<_>>();
    if parts.len() != 4
        || parts[0] != "00"
        || parts[1].len() != 32
        || parts[2].len() != 16
        || parts[3].len() != 2
        || !parts.iter().all(|part| {
            part.bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        })
        || parts[1] == "00000000000000000000000000000000"
        || parts[2] == "0000000000000000"
    {
        return Err(Status::invalid_argument("invalid traceparent"));
    }
    Ok(())
}

fn new_traceparent() -> String {
    let trace_id = Uuid::now_v7();
    let span_id = Uuid::now_v7();
    format!(
        "00-{}-{}-01",
        hex::encode(trace_id.as_bytes()),
        hex::encode(&span_id.as_bytes()[..8])
    )
}

fn now_timestamp() -> prost_types::Timestamp {
    SystemTime::now().into()
}

/// Builds the single controller endpoint and registers exactly the two platform ALPNs.
///
/// # Errors
///
/// Returns an Iroh bind error when endpoint or transport initialization fails.
pub async fn build_router(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    service: &IrohTransportService,
) -> Result<Router, iroh::endpoint::BindError> {
    build_router_with_direct(secret_key, relay_mode, policy, service, true).await
}

async fn build_router_with_direct(
    secret_key: SecretKey,
    relay_mode: RelayMode,
    policy: IdentityPolicy,
    service: &IrohTransportService,
    direct_enabled: bool,
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
        .transport_config(transport)
        .hooks(SecurityHook::new(policy.clone()));
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
            },
        )
        .accept(
            AGENT_ALPN,
            SessionHandler {
                shared: service.shared.clone(),
                policy,
                kind: ProtocolKind::Agent,
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
mod tests {
    use super::*;

    fn handshake(key: &SecretKey) -> SessionHandshake {
        SessionHandshake {
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            agent_version: "test".to_owned(),
            controller_version: String::new(),
            node_id: Uuid::now_v7().as_bytes().to_vec(),
            endpoint_id: key.public().as_bytes().to_vec(),
            capabilities: vec!["ocserv.status.read".to_owned()],
            ocserv_version: "1.3".to_owned(),
            os_release: "test".to_owned(),
            boot_id: "boot".to_owned(),
            agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
            supported_compressions: Vec::new(),
            max_message_size: MAX_FRAME_BYTES_U32,
            time: Some(now_timestamp()),
            nonce: vec![7; 32],
        }
    }

    fn identity_policy(key: &SecretKey, handshake: &SessionHandshake) -> IdentityPolicy {
        IdentityPolicy::new(
            HashMap::from([(key.public(), handshake.node_id.clone())]),
            HashSet::new(),
        )
    }

    fn command_envelope(node_id: &[u8], traceparent: &str, reason: String) -> Vec<u8> {
        CommandEnvelope {
            protocol_version: "1.0".to_owned(),
            message_id: Uuid::now_v7().as_bytes().to_vec(),
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key: Uuid::now_v7().as_bytes().to_vec(),
            node_id: node_id.to_vec(),
            sequence: 1,
            issued_at: Some(now_timestamp()),
            expires_at: Some((SystemTime::now() + Duration::from_secs(30)).into()),
            expected_revision: 0,
            traceparent: traceparent.to_owned(),
            actor_id: "test".to_owned(),
            reason,
            payload: None,
        }
        .encode_to_vec()
    }

    async fn send_handshake(
        connection: &Connection,
        handshake: &SessionHandshake,
    ) -> SessionHandshakeResponse {
        let bytes = handshake.encode_to_vec();
        let (mut send, mut recv) = connection.open_bi().await.expect("open handshake stream");
        let request_len = u32::try_from(bytes.len()).expect("test handshake length fits u32");
        send.write_all(&request_len.to_be_bytes())
            .await
            .expect("write handshake length");
        send.write_all(&bytes).await.expect("write handshake");
        send.finish().expect("finish handshake request");
        let mut length = [0_u8; 4];
        recv.read_exact(&mut length)
            .await
            .expect("read response length");
        let mut response = vec![0_u8; u32::from_be_bytes(length) as usize];
        recv.read_exact(&mut response).await.expect("read response");
        SessionHandshakeResponse::decode(response.as_slice()).expect("decode response")
    }

    async fn respond_to_command(connection: Connection) {
        let (mut send, mut recv) = connection.accept_bi().await.expect("accept command");
        let mut length = [0_u8; 4];
        recv.read_exact(&mut length)
            .await
            .expect("read command length");
        let mut command = vec![0_u8; u32::from_be_bytes(length) as usize];
        recv.read_exact(&mut command).await.expect("read command");
        CommandEnvelope::decode(command.as_slice()).expect("decode command");
        let response = AgentEvent {
            r#type: AgentEventType::CommandResult.into(),
            payload: b"completed".to_vec(),
        }
        .encode_to_vec();
        let response_len = u32::try_from(response.len()).expect("response length fits u32");
        send.write_all(&response_len.to_be_bytes())
            .await
            .expect("write response length");
        send.write_all(&response).await.expect("write response");
        send.finish().expect("finish response");
    }

    async fn end_command_without_response(connection: Connection) {
        let (mut send, mut recv) = connection.accept_bi().await.expect("accept command");
        let mut length = [0_u8; 4];
        recv.read_exact(&mut length)
            .await
            .expect("read command length");
        let mut command = vec![0_u8; u32::from_be_bytes(length) as usize];
        recv.read_exact(&mut command).await.expect("read command");
        CommandEnvelope::decode(command.as_slice()).expect("decode command");
        send.finish().expect("finish empty response");
    }

    async fn next_event(
        events: &mut EventStream,
        event_type: TransportEventType,
    ) -> TransportEvent {
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let event = events
                    .next()
                    .await
                    .expect("event stream remains open")
                    .expect("event is valid");
                if event.r#type == i32::from(event_type) {
                    break event;
                }
            }
        })
        .await
        .expect("event arrives")
    }

    async fn wait_until_registered(service: &IrohTransportService, node_id: &[u8]) {
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if service
                    .shared
                    .inner
                    .connections
                    .lock()
                    .await
                    .contains_key(node_id)
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("connection registered");
    }

    async fn last_seen(
        service: &IrohTransportService,
        node_id: &[u8],
    ) -> Option<prost_types::Timestamp> {
        service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: node_id.to_vec(),
            }))
            .await
            .expect("query connection")
            .into_inner()
            .last_seen
    }

    #[test]
    fn policy_rejects_unknown_agent_and_revoked_enrollment() {
        let approved = SecretKey::generate().public();
        let revoked = SecretKey::generate().public();
        let approved_node = Uuid::now_v7().as_bytes().to_vec();
        let policy = IdentityPolicy::new(
            HashMap::from([(approved, approved_node.clone())]),
            HashSet::from([revoked]),
        );
        assert!(policy.permits(approved, AGENT_ALPN));
        assert!(policy.matches_node(approved, &approved_node));
        assert!(!policy.matches_node(approved, Uuid::now_v7().as_bytes()));
        assert!(!policy.permits(revoked, ENROLL_ALPN));
        assert!(!policy.permits(SecretKey::generate().public(), AGENT_ALPN));
    }

    #[test]
    fn handshake_limits_reject_identity_and_version_mismatch() {
        let remote_key = SecretKey::generate();
        let mut handshake = SessionHandshake {
            protocol_major: PROTOCOL_MAJOR,
            protocol_minor: PROTOCOL_MINOR,
            agent_version: "test".to_owned(),
            controller_version: String::new(),
            node_id: Uuid::now_v7().as_bytes().to_vec(),
            endpoint_id: remote_key.public().as_bytes().to_vec(),
            capabilities: Vec::new(),
            ocserv_version: String::new(),
            os_release: String::new(),
            boot_id: "boot".to_owned(),
            agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
            supported_compressions: Vec::new(),
            max_message_size: MAX_FRAME_BYTES_U32,
            time: Some(now_timestamp()),
            nonce: vec![7; 32],
        };
        assert!(validate_handshake(&handshake, remote_key.public()).is_ok());
        handshake.protocol_major = 2;
        assert!(validate_handshake(&handshake, remote_key.public()).is_err());
        handshake.protocol_major = PROTOCOL_MAJOR;
        assert!(validate_handshake(&handshake, SecretKey::generate().public()).is_err());
    }

    #[test]
    fn queues_are_bounded() {
        let service = IrohTransportService::new(usize::MAX);
        assert_eq!(service.shared.inner.event_capacity, MAX_CONNECTIONS);
        assert_eq!(
            service
                .shared
                .inner
                .agent_connection_permits
                .available_permits(),
            MAX_AGENT_CONNECTIONS
        );
        assert_eq!(
            service
                .shared
                .inner
                .enrollment_connection_permits
                .available_permits(),
            MAX_ENROLLMENT_CONNECTIONS
        );
    }

    #[test]
    fn enrollment_capacity_cannot_exhaust_agent_capacity() {
        let service = IrohTransportService::new(8);
        let enrollment_permits = service
            .shared
            .inner
            .enrollment_connection_permits
            .clone()
            .try_acquire_many_owned(
                u32::try_from(MAX_ENROLLMENT_CONNECTIONS).expect("enrollment capacity fits u32"),
            )
            .expect("reserve all enrollment permits");

        assert_eq!(
            service
                .shared
                .inner
                .enrollment_connection_permits
                .available_permits(),
            0
        );
        assert_eq!(
            service
                .shared
                .inner
                .agent_connection_permits
                .available_permits(),
            MAX_AGENT_CONNECTIONS
        );
        drop(enrollment_permits);
    }

    #[test]
    fn transport_events_have_a_valid_root_traceparent() {
        let traceparent = event(
            Uuid::now_v7().as_bytes(),
            TransportEventType::Connected,
            Vec::new(),
        )
        .traceparent;
        let parts = traceparent.split('-').collect::<Vec<_>>();
        assert_eq!(parts.len(), 4);
        assert_eq!(parts[0], "00");
        assert_eq!(parts[1].len(), 32);
        assert_eq!(parts[2].len(), 16);
        assert_eq!(parts[3], "01");
        assert!(parts[1..=2].iter().all(|part| {
            part.bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        }));
    }

    #[tokio::test]
    async fn shutdown_terminates_event_streams() {
        let service = IrohTransportService::new(8);
        let mut stream = service
            .watch_events(Request::new(WatchEventsRequest {
                after_event_id: Vec::new(),
            }))
            .await
            .expect("open event stream")
            .into_inner();
        service.begin_shutdown().await;
        assert!(
            tokio::time::timeout(Duration::from_secs(1), stream.next())
                .await
                .expect("stream terminates promptly")
                .is_none()
        );
    }

    #[tokio::test]
    async fn router_rejects_wrong_alpn_and_unknown_agent() {
        let service = IrohTransportService::new(8);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Disabled,
            IdentityPolicy::default(),
            &service,
        )
        .await
        .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let address = router.endpoint().addr();

        assert!(
            client
                .connect(address.clone(), b"wrong/alpn/1")
                .await
                .is_err()
        );
        let unknown = client
            .connect(address, AGENT_ALPN)
            .await
            .expect("TLS identity is established before the endpoint hook rejects it");
        tokio::time::timeout(Duration::from_secs(2), unknown.closed())
            .await
            .expect("unknown endpoint closed before application data");

        client.close().await;
        shutdown(&service, router).await.expect("shutdown router");
    }

    #[tokio::test]
    async fn direct_connection_is_registered_and_shutdown_is_graceful() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let service = IrohTransportService::new(8);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Disabled,
            identity_policy(&agent_key, &handshake),
            &service,
        )
        .await
        .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let connection = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect agent");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));

        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if service
                    .shared
                    .inner
                    .connections
                    .lock()
                    .await
                    .contains_key(&handshake.node_id)
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("connection registered");
        let metadata = service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: handshake.node_id,
            }))
            .await
            .expect("query connection")
            .into_inner();
        assert_eq!(metadata.endpoint_id, agent_key.public().as_bytes());
        assert_eq!(metadata.path, i32::from(ConnectionPath::Direct));

        shutdown(&service, router).await.expect("shutdown router");
        tokio::time::timeout(Duration::from_secs(2), connection.closed())
            .await
            .expect("connection closed after shutdown");
        client.close().await;
    }

    #[tokio::test]
    async fn command_responses_are_published_with_negotiated_limits() {
        let agent_key = SecretKey::generate();
        let mut handshake = handshake(&agent_key);
        handshake.max_message_size = 256;
        let service = IrohTransportService::new(16);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Disabled,
            identity_policy(&agent_key, &handshake),
            &service,
        )
        .await
        .expect("build router");
        let client = Endpoint::builder(presets::Minimal)
            .secret_key(agent_key.clone())
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build client");
        let connection = client
            .connect(router.endpoint().addr(), AGENT_ALPN)
            .await
            .expect("connect agent");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
        wait_until_registered(&service, &handshake.node_id).await;

        let traceparent = new_traceparent();
        let baseline_last_seen = last_seen(&service, &handshake.node_id).await;
        let oversized = command_envelope(&handshake.node_id, &traceparent, "x".repeat(256));
        let error = service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: oversized,
            }))
            .await
            .expect_err("negotiated limit enforced");
        assert_eq!(error.code(), tonic::Code::ResourceExhausted);
        assert_eq!(
            last_seen(&service, &handshake.node_id).await,
            baseline_last_seen
        );

        tokio::time::sleep(Duration::from_millis(2)).await;
        let agent = tokio::spawn(respond_to_command(connection.clone()));
        let mut events = service
            .watch_events(Request::new(WatchEventsRequest {
                after_event_id: Vec::new(),
            }))
            .await
            .expect("watch events")
            .into_inner();
        let command = command_envelope(&handshake.node_id, &traceparent, String::new());
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: command,
            }))
            .await
            .expect("send command");
        agent.await.expect("agent response task");
        let event = next_event(&mut events, TransportEventType::CommandResult).await;
        assert_eq!(event.node_id, handshake.node_id);
        assert_eq!(event.traceparent, traceparent);
        assert_eq!(event.payload, b"completed");
        assert_ne!(
            last_seen(&service, &handshake.node_id).await,
            baseline_last_seen
        );

        let eof_traceparent = new_traceparent();
        let agent = tokio::spawn(end_command_without_response(connection.clone()));
        service
            .send_command(Request::new(SendCommandRequest {
                node_id: handshake.node_id.clone(),
                command_envelope: command_envelope(
                    &handshake.node_id,
                    &eof_traceparent,
                    String::new(),
                ),
            }))
            .await
            .expect("send command with empty response");
        agent.await.expect("empty response task");
        let event = next_event(&mut events, TransportEventType::Error).await;
        assert_eq!(event.traceparent, eof_traceparent);
        assert_eq!(
            event.payload,
            b"agent response ended before a terminal event"
        );

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }

    #[tokio::test]
    #[ignore = "requires access to the configured public Iroh relay"]
    async fn relay_only_connection_and_disabled_relay_failure() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let service = IrohTransportService::new(8);
        let router = build_router_with_direct(
            SecretKey::generate(),
            RelayMode::Default,
            identity_policy(&agent_key, &handshake),
            &service,
            false,
        )
        .await
        .expect("build router");
        tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
            .await
            .expect("controller endpoint online");
        let full_address = router.endpoint().addr();
        let relay_url = full_address
            .relay_urls()
            .next()
            .expect("controller selected a home relay")
            .clone();
        let relay_only = iroh::EndpointAddr::new(full_address.id).with_relay_url(relay_url);

        let disabled = Endpoint::builder(presets::Minimal)
            .relay_mode(RelayMode::Disabled)
            .bind()
            .await
            .expect("build relay-disabled client");
        let disabled_attempt = tokio::time::timeout(
            Duration::from_secs(5),
            disabled.connect(relay_only.clone(), AGENT_ALPN),
        )
        .await;
        assert!(!matches!(disabled_attempt, Ok(Ok(_))));
        disabled.close().await;

        let client = Endpoint::builder(presets::N0)
            .secret_key(agent_key.clone())
            .clear_address_lookup()
            .clear_ip_transports()
            .bind()
            .await
            .expect("build relay client");
        let connection = tokio::time::timeout(
            Duration::from_secs(30),
            client.connect(relay_only, AGENT_ALPN),
        )
        .await
        .expect("relay connection completed")
        .expect("connect over relay");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let path = service
                    .shared
                    .inner
                    .connections
                    .lock()
                    .await
                    .get(&handshake.node_id)
                    .map(|entry| entry.metadata.path);
                if path == Some(i32::from(ConnectionPath::Relay)) {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("relay path registered");

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }

    #[tokio::test]
    #[ignore = "requires access to the configured public Iroh relay"]
    async fn relay_and_direct_paths_converge_to_direct() {
        let agent_key = SecretKey::generate();
        let handshake = handshake(&agent_key);
        let service = IrohTransportService::new(16);
        let router = build_router(
            SecretKey::generate(),
            RelayMode::Default,
            identity_policy(&agent_key, &handshake),
            &service,
        )
        .await
        .expect("build router");
        tokio::time::timeout(Duration::from_secs(30), router.endpoint().online())
            .await
            .expect("controller endpoint online");
        let client = Endpoint::builder(presets::N0)
            .secret_key(agent_key.clone())
            .bind()
            .await
            .expect("build client");
        let connection = tokio::time::timeout(
            Duration::from_secs(30),
            client.connect(router.endpoint().addr(), AGENT_ALPN),
        )
        .await
        .expect("connection completed")
        .expect("connect agent");
        let response = send_handshake(&connection, &handshake).await;
        assert_eq!(response.result, i32::from(HandshakeResult::Accepted));

        tokio::time::timeout(Duration::from_secs(30), async {
            loop {
                let paths = connection.paths();
                let has_relay = paths.iter().any(|path| path.is_relay());
                let direct_selected = paths.iter().any(|path| path.is_ip() && path.is_selected());
                if has_relay && direct_selected {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        })
        .await
        .expect("relay fallback and selected direct path observed");
        let metadata = service
            .get_node_connection(Request::new(GetNodeConnectionRequest {
                node_id: handshake.node_id,
            }))
            .await
            .expect("query connection")
            .into_inner();
        assert_eq!(metadata.path, i32::from(ConnectionPath::Direct));

        shutdown(&service, router).await.expect("shutdown router");
        client.close().await;
    }
}
