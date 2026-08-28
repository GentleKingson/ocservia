//! Side-effect-free transport stub and bounded multi-node agent simulator.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet, VecDeque};
use std::pin::Pin;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ocservia_contracts::decode_strict_command_envelope;
use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    ArtifactChunk, CommandDeliveryMode, ConnectionFenceV2, MetricSample, ObservedSnapshot,
    SimulationProbe, TelemetryBatch, TelemetryDropCounters, TelemetryPriority, command_envelope,
};
use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    CloseNodeRequest, CloseNodeResponse, ConnectionPath, ConsumeArtifactRequest,
    ConsumeArtifactResponse, FetchArtifactRequest, GetNodeConnectionRequest, GetOwnerFenceRequest,
    GetOwnerFenceResponse, HealthRequest, HealthResponse, HealthStatus, NodeConnection,
    OwnerFenceDisposition, RegisterOwnerFenceRequest, RegisterOwnerFenceResponse,
    SendCommandRequest, SendCommandResponse, TransportEvent, TransportEventType,
    TrustUpdateDisposition, UpdateNodeTrustRequest, UpdateNodeTrustResponse, WatchEventsRequest,
    transport_service_server::TransportService,
};
use prost::Message;
use sha2::{Digest as _, Sha256};
use tokio::sync::{Mutex, Semaphore, mpsc, watch};
use tokio_stream::{Stream, wrappers::ReceiverStream};
use tonic::{Request, Response, Status};
use uuid::Uuid;

const MAX_COMMAND_BYTES: usize = 1024 * 1024;
const MAX_HEARTBEATS: u32 = 32;
const MAX_DELAY: Duration = Duration::from_secs(30);

type EventStream = Pin<Box<dyn Stream<Item = Result<TransportEvent, Status>> + Send>>;
type ArtifactStream = Pin<Box<dyn Stream<Item = Result<ArtifactChunk, Status>> + Send>>;

#[derive(Clone)]
pub struct StubService {
    state: Arc<State>,
}

struct State {
    delivery: Mutex<EventState>,
    nodes: Mutex<HashMap<Vec<u8>, ActiveNode>>,
    fences: Mutex<HashMap<Vec<u8>, RecordedFence>>,
    accepted_commands: Mutex<AcceptedCommands>,
    retention: usize,
    tasks: Arc<Semaphore>,
    capacity_telemetry: bool,
}

/// The stub's owner-fence bookkeeping. This simulator performs no Controller
/// signature verification and applies no lease clock; it records only the
/// epoch watermark so development flows observe the same disposition
/// semantics as transportd. Real enforcement lives in transportd.
struct RecordedFence {
    fence_id: Vec<u8>,
    owner_epoch: u64,
    fence: ConnectionFenceV2,
}

struct EventState {
    retained: VecDeque<TransportEvent>,
    subscribers: Vec<mpsc::Sender<Result<TransportEvent, Status>>>,
}

struct AcceptedCommands {
    keys: HashSet<Vec<u8>>,
}

struct ActiveNode {
    connection: NodeConnection,
    generation: Uuid,
    cancellation: watch::Sender<bool>,
    traceparent: String,
}

impl StubService {
    /// Creates a service with bounded replay and subscriber queues.
    #[must_use]
    pub fn new(queue_capacity: usize) -> Self {
        Self::new_with_capacity_telemetry(queue_capacity, false)
    }

    /// Creates a stub that emits representative telemetry for capacity tests.
    #[must_use]
    pub fn new_capacity(queue_capacity: usize) -> Self {
        Self::new_with_capacity_telemetry(queue_capacity, true)
    }

    fn new_with_capacity_telemetry(queue_capacity: usize, capacity_telemetry: bool) -> Self {
        let capacity = queue_capacity.clamp(1, 4096);
        Self {
            state: Arc::new(State {
                delivery: Mutex::new(EventState {
                    retained: VecDeque::with_capacity(capacity),
                    subscribers: Vec::new(),
                }),
                nodes: Mutex::new(HashMap::new()),
                fences: Mutex::new(HashMap::new()),
                accepted_commands: Mutex::new(AcceptedCommands {
                    keys: HashSet::with_capacity(capacity),
                }),
                retention: capacity,
                tasks: Arc::new(Semaphore::new(capacity)),
                capacity_telemetry,
            }),
        }
    }

    /// Returns bounded runtime counters for the repeatable capacity harness.
    pub async fn stats(&self) -> StubStats {
        let delivery = self.state.delivery.lock().await;
        let retained_events = delivery.retained.len();
        let subscribers = delivery.subscribers.len();
        drop(delivery);
        let connected_nodes = self.state.nodes.lock().await.len();
        let accepted_commands = self.state.accepted_commands.lock().await.keys.len();
        StubStats {
            connected_nodes,
            retained_events,
            subscribers,
            accepted_commands,
            active_tasks: self.state.retention - self.state.tasks.available_permits(),
            task_capacity: self.state.retention,
        }
    }

    /// Stamps the node's recorded owner term onto an event so the simulator
    /// keeps the same disconnect correlation the real transportd provides.
    async fn with_registered_term(&self, mut event: TransportEvent) -> TransportEvent {
        let fences = self.state.fences.lock().await;
        if let Some(recorded) = fences.get(&event.node_id) {
            event.connection_id = recorded.fence.connection_id.clone();
            event.owner_epoch = recorded.fence.owner_epoch;
        }
        event
    }

    async fn publish(&self, event: TransportEvent) {
        let event = self.with_registered_term(event).await;
        let mut state = self.state.delivery.lock().await;
        if state.retained.len() == self.state.retention {
            state.retained.pop_front();
        }
        state.retained.push_back(event.clone());
        let mut index = 0;
        while index < state.subscribers.len() {
            if state.subscribers[index]
                .send(Ok(event.clone()))
                .await
                .is_err()
            {
                state.subscribers.swap_remove(index);
            } else {
                index += 1;
            }
        }
    }

    async fn publish_control(&self, event: TransportEvent) {
        let event = self.with_registered_term(event).await;
        let mut state = self.state.delivery.lock().await;
        if state.retained.len() == self.state.retention {
            state.retained.pop_front();
        }
        state.retained.push_back(event.clone());
        let mut index = 0;
        while index < state.subscribers.len() {
            if state.subscribers[index]
                .try_send(Ok(event.clone()))
                .is_err()
            {
                state.subscribers.swap_remove(index);
            } else {
                index += 1;
            }
        }
    }

    async fn publish_if_active(
        &self,
        node_id: &[u8],
        generation: Uuid,
        event: TransportEvent,
    ) -> bool {
        let mut cancelled = {
            let nodes = self.state.nodes.lock().await;
            let Some(node) = nodes
                .get(node_id)
                .filter(|node| node.generation == generation)
            else {
                return false;
            };
            node.cancellation.subscribe()
        };
        if *cancelled.borrow() {
            return false;
        }
        let event = self.with_registered_term(event).await;
        let mut state = self.state.delivery.lock().await;
        if *cancelled.borrow() {
            return false;
        }
        if state.retained.len() == self.state.retention {
            state.retained.pop_front();
        }
        state.retained.push_back(event.clone());
        let mut index = 0;
        while index < state.subscribers.len() {
            tokio::select! {
                biased;
                result = cancelled.changed() => {
                    if result.is_err() || *cancelled.borrow() {
                        state.subscribers.truncate(index);
                        return false;
                    }
                }
                result = state.subscribers[index].send(Ok(event.clone())) => {
                    if result.is_err() {
                        state.subscribers.swap_remove(index);
                    } else {
                        index += 1;
                    }
                }
            }
        }
        true
    }

    #[allow(clippy::too_many_lines)]
    async fn run_probe(self, node_id: Vec<u8>, traceparent: String, probe: SimulationProbe) {
        let delay = Duration::from_millis(u64::from(probe.delay_millis));
        let connected_at = now_timestamp();
        let generation = Uuid::now_v7();
        let agent_instance_id = Uuid::now_v7();
        let path = if self.state.capacity_telemetry {
            capacity_path(&node_id)
        } else {
            ConnectionPath::Direct
        };
        let (cancellation, mut cancelled) = watch::channel(false);
        self.state.nodes.lock().await.insert(
            node_id.clone(),
            ActiveNode {
                connection: NodeConnection {
                    node_id: node_id.clone(),
                    endpoint_id: simulator_endpoint(&node_id),
                    path: path.into(),
                    round_trip_time_millis: probe.delay_millis.into(),
                    connected_at: Some(connected_at),
                    agent_instance_id: agent_instance_id.as_bytes().to_vec(),
                    path_detail: capacity_path_detail(path).to_owned(),
                    last_seen: Some(now_timestamp()),
                    negotiated_capabilities: Vec::new(),
                    authorization_revision: 0,
                    session_expires_at: None,
                    owner_epoch: 0,
                },
                generation,
                cancellation,
                traceparent: traceparent.clone(),
            },
        );
        if !self
            .publish_if_active(
                &node_id,
                generation,
                new_event(
                    &node_id,
                    TransportEventType::Connected,
                    &traceparent,
                    b"connected".to_vec(),
                ),
            )
            .await
        {
            return;
        }

        for sequence in 0..probe.heartbeat_count.max(1) {
            tokio::select! {
                result = cancelled.changed() => {
                    if result.is_err() || *cancelled.borrow() {
                        return;
                    }
                }
                () = tokio::time::sleep(delay) => {}
            }
            let event = new_event(
                &node_id,
                TransportEventType::Heartbeat,
                &traceparent,
                format!("heartbeat:{sequence}").into_bytes(),
            );
            if !self
                .publish_if_active(&node_id, generation, event.clone())
                .await
            {
                return;
            }
            if probe.duplicate_event && !self.publish_if_active(&node_id, generation, event).await {
                return;
            }
            if self.state.capacity_telemetry
                && !self
                    .publish_if_active(
                        &node_id,
                        generation,
                        new_telemetry_event(
                            &node_id,
                            agent_instance_id,
                            path,
                            sequence,
                            probe.delay_millis,
                            &traceparent,
                        ),
                    )
                    .await
            {
                return;
            }
        }

        let (event_type, payload) = if probe.return_error {
            (TransportEventType::Error, b"simulated error".to_vec())
        } else {
            (TransportEventType::SimulationResult, b"completed".to_vec())
        };
        if !self
            .publish_if_active(
                &node_id,
                generation,
                new_event(&node_id, event_type, &traceparent, payload),
            )
            .await
        {
            return;
        }

        if probe.disconnect_after {
            let removed = {
                let mut nodes = self.state.nodes.lock().await;
                if nodes
                    .get(&node_id)
                    .is_some_and(|node| node.generation == generation)
                {
                    nodes.remove(&node_id)
                } else {
                    None
                }
            };
            if removed.is_some() {
                self.publish(new_event(
                    &node_id,
                    TransportEventType::Disconnected,
                    &traceparent,
                    b"simulated disconnect".to_vec(),
                ))
                .await;
            }
        }
    }
}

/// Snapshot of the simulator's bounded in-memory state.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StubStats {
    pub connected_nodes: usize,
    pub retained_events: usize,
    pub subscribers: usize,
    pub accepted_commands: usize,
    pub active_tasks: usize,
    pub task_capacity: usize,
}

fn capacity_path(node_id: &[u8]) -> ConnectionPath {
    if node_id.last().is_some_and(|value| value % 4 == 0) {
        ConnectionPath::Relay
    } else {
        ConnectionPath::Direct
    }
}

fn capacity_path_detail(path: ConnectionPath) -> &'static str {
    match path {
        ConnectionPath::Relay => "stub/relay-a",
        _ => "stub/direct",
    }
}

fn new_telemetry_event(
    node_id: &[u8],
    agent_instance_id: Uuid,
    path: ConnectionPath,
    sequence: u32,
    round_trip_time_millis: u32,
    traceparent: &str,
) -> TransportEvent {
    let observed_at = now_timestamp();
    let path_name = if path == ConnectionPath::Relay {
        "relay"
    } else {
        "direct"
    };
    let batch = TelemetryBatch {
        batch_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        sequence: u64::from(sequence) + 1,
        priority: TelemetryPriority::CurrentHealth.into(),
        snapshot: Some(ObservedSnapshot {
            observed_at: Some(observed_at),
            boot_id: "capacity-boot".to_owned(),
            agent_instance_id: agent_instance_id.as_bytes().to_vec(),
            agent_version: env!("CARGO_PKG_VERSION").to_owned(),
            upgrade_results: Vec::new(),
            architecture: "amd64".to_owned(),
            ocserv_version: "1.3.0".to_owned(),
            os_release: "capacity-simulator".to_owned(),
            ocserv_json: br#"{"status":"running","sessions":1}"#.to_vec(),
            system_json: br#"{"cpu_usage_ratio":0.25,"memory_used_bytes":67108864}"#.to_vec(),
            path_json: format!("{{\"mode\":\"{path_name}\",\"rtt_ms\":{round_trip_time_millis}}}")
                .into_bytes(),
            dropped: Some(TelemetryDropCounters::default()),
        }),
        sessions: Vec::new(),
        samples: vec![
            MetricSample {
                sampled_at: Some(observed_at),
                metric: "cpu_usage_ratio".to_owned(),
                value: 0.25,
            },
            MetricSample {
                sampled_at: Some(observed_at),
                metric: "connection_rtt_ms".to_owned(),
                value: f64::from(round_trip_time_millis),
            },
        ],
        security_events: Vec::new(),
        ip_bans: Vec::new(),
        users: Vec::new(),
        groups: Vec::new(),
    };
    new_event(
        node_id,
        TransportEventType::Telemetry,
        traceparent,
        batch.encode_to_vec(),
    )
}

impl StubService {
    async fn accept_synthetic(
        &self,
        idempotency_key: Vec<u8>,
    ) -> Result<Response<SendCommandResponse>, Status> {
        let mut accepted = self.state.accepted_commands.lock().await;
        if accepted.keys.contains(&idempotency_key) {
            return Ok(Response::new(SendCommandResponse { accepted: true }));
        }
        if accepted.keys.len() == self.state.retention {
            return Err(Status::resource_exhausted(
                "idempotency capacity reached; restart the development stub",
            ));
        }
        accepted.keys.insert(idempotency_key);
        Ok(Response::new(SendCommandResponse { accepted: true }))
    }
}

#[tonic::async_trait]
impl TransportService for StubService {
    type WatchEventsStream = EventStream;
    type FetchArtifactStream = ArtifactStream;

    async fn fetch_artifact(
        &self,
        _request: Request<FetchArtifactRequest>,
    ) -> Result<Response<Self::FetchArtifactStream>, Status> {
        Err(Status::unavailable(
            "artifact streaming requires a connected real agent",
        ))
    }

    async fn consume_artifact(
        &self,
        _request: Request<ConsumeArtifactRequest>,
    ) -> Result<Response<ConsumeArtifactResponse>, Status> {
        Err(Status::unavailable(
            "artifact consumption requires a connected real agent",
        ))
    }

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
        let node_id = validate_id(&request.into_inner().node_id, "node_id")?;
        let connection = self
            .state
            .nodes
            .lock()
            .await
            .get(&node_id)
            .map(|node| node.connection.clone())
            .ok_or_else(|| Status::not_found("node is not connected"))?;
        Ok(Response::new(connection))
    }

    async fn send_command(
        &self,
        request: Request<SendCommandRequest>,
    ) -> Result<Response<SendCommandResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_id(&request.node_id, "node_id")?;
        if request.command_envelope.len() > MAX_COMMAND_BYTES {
            return Err(Status::resource_exhausted("command exceeds 1 MiB"));
        }
        let envelope = decode_strict_command_envelope(request.command_envelope.as_slice())
            .map_err(|_| Status::invalid_argument("invalid or unknown command envelope fields"))?;
        if envelope.node_id != node_id {
            return Err(Status::invalid_argument(
                "command node_id does not match request",
            ));
        }
        validate_traceparent(&envelope.traceparent)?;
        validate_command_times(envelope.issued_at.as_ref(), envelope.expires_at.as_ref())?;
        if CommandDeliveryMode::try_from(envelope.delivery_mode)
            .unwrap_or(CommandDeliveryMode::Unspecified)
            == CommandDeliveryMode::Unspecified
        {
            return Err(Status::invalid_argument(
                "command delivery mode is required",
            ));
        }
        let idempotency_key = validate_id(&envelope.idempotency_key, "idempotency_key")?;
        let payload = envelope
            .payload
            .ok_or_else(|| Status::invalid_argument("command payload is required"))?;
        let probe = match payload {
            command_envelope::Payload::SimulationProbe(probe) => probe,
            command_envelope::Payload::SyntheticNoop(_) => {
                return self.accept_synthetic(idempotency_key).await;
            }
            command_envelope::Payload::SyntheticEcho(echo) => {
                if echo.message.len() > 4096 {
                    return Err(Status::invalid_argument(
                        "synthetic echo exceeds 4096 bytes",
                    ));
                }
                return self.accept_synthetic(idempotency_key).await;
            }
            _ => return Err(Status::unimplemented("stub rejects non-synthetic commands")),
        };
        if probe.heartbeat_count > MAX_HEARTBEATS
            || Duration::from_millis(u64::from(probe.delay_millis)) > MAX_DELAY
        {
            return Err(Status::invalid_argument("simulation limits exceeded"));
        }
        let mut accepted = self.state.accepted_commands.lock().await;
        if accepted.keys.contains(&idempotency_key) {
            return Ok(Response::new(SendCommandResponse { accepted: true }));
        }
        if accepted.keys.len() == self.state.retention {
            return Err(Status::resource_exhausted(
                "idempotency capacity reached; restart the development stub",
            ));
        }
        let permit = self
            .state
            .tasks
            .clone()
            .try_acquire_owned()
            .map_err(|_| Status::resource_exhausted("simulator task capacity reached"))?;
        accepted.keys.insert(idempotency_key.clone());
        drop(accepted);
        let service = self.clone();
        tokio::spawn(async move {
            service
                .run_probe(node_id, envelope.traceparent, probe)
                .await;
            drop(permit);
        });
        Ok(Response::new(SendCommandResponse { accepted: true }))
    }

    async fn close_node(
        &self,
        request: Request<CloseNodeRequest>,
    ) -> Result<Response<CloseNodeResponse>, Status> {
        let request = request.into_inner();
        let node_id = validate_id(&request.node_id, "node_id")?;
        if request.reason.len() > MAX_COMMAND_BYTES {
            return Err(Status::resource_exhausted("close reason exceeds 1 MiB"));
        }
        let node = self
            .state
            .nodes
            .lock()
            .await
            .remove(&node_id)
            .ok_or_else(|| Status::not_found("node is not connected"))?;
        node.cancellation.send_replace(true);
        self.publish_control(new_event(
            &node_id,
            TransportEventType::Disconnected,
            &node.traceparent,
            request.reason.into_bytes(),
        ))
        .await;
        Ok(Response::new(CloseNodeResponse {}))
    }

    async fn update_node_trust(
        &self,
        request: Request<UpdateNodeTrustRequest>,
    ) -> Result<Response<UpdateNodeTrustResponse>, Status> {
        let request = request.into_inner();
        validate_id(&request.node_id, "node_id")?;
        if request.endpoint_id.len() != 32
            || request.state == 0
            || request.reason.is_empty()
            || request.revision == 0
        {
            return Err(Status::invalid_argument("trust update is invalid"));
        }
        Ok(Response::new(UpdateNodeTrustResponse {
            disposition: TrustUpdateDisposition::Applied.into(),
            retained_revision: request.revision,
            retained_state: request.state,
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
        let node_id = validate_id(&fence.node_id, "node_id")?;
        if fence.fence_id.len() != 16 {
            return Err(Status::invalid_argument("fence_id must be 16 bytes"));
        }
        let mut fences = self.state.fences.lock().await;
        match fences.get(&node_id) {
            Some(existing) if existing.owner_epoch > fence.owner_epoch => {
                let retained_epoch = existing.owner_epoch;
                Ok(Response::new(RegisterOwnerFenceResponse {
                    disposition: OwnerFenceDisposition::Stale.into(),
                    retained_epoch,
                }))
            }
            Some(existing)
                if existing.owner_epoch == fence.owner_epoch
                    && existing.fence_id != fence.fence_id =>
            {
                Err(Status::invalid_argument(
                    "owner epoch is already claimed by a different fence",
                ))
            }
            Some(existing) if existing.fence_id == fence.fence_id => {
                let retained_epoch = existing.owner_epoch;
                fences.insert(
                    node_id,
                    RecordedFence {
                        fence_id: fence.fence_id.clone(),
                        owner_epoch: fence.owner_epoch,
                        fence,
                    },
                );
                Ok(Response::new(RegisterOwnerFenceResponse {
                    disposition: OwnerFenceDisposition::Refreshed.into(),
                    retained_epoch,
                }))
            }
            _ => {
                let retained_epoch = fence.owner_epoch;
                fences.insert(
                    node_id,
                    RecordedFence {
                        fence_id: fence.fence_id.clone(),
                        owner_epoch: fence.owner_epoch,
                        fence,
                    },
                );
                Ok(Response::new(RegisterOwnerFenceResponse {
                    disposition: OwnerFenceDisposition::Applied.into(),
                    retained_epoch,
                }))
            }
        }
    }

    async fn get_owner_fence(
        &self,
        request: Request<GetOwnerFenceRequest>,
    ) -> Result<Response<GetOwnerFenceResponse>, Status> {
        let node_id = validate_id(&request.into_inner().node_id, "node_id")?;
        let fence = self
            .state
            .fences
            .lock()
            .await
            .get(&node_id)
            .map(|recorded| recorded.fence.clone())
            .ok_or_else(|| Status::not_found("owner fence is not registered"))?;
        Ok(Response::new(GetOwnerFenceResponse { fence: Some(fence) }))
    }

    async fn watch_events(
        &self,
        request: Request<WatchEventsRequest>,
    ) -> Result<Response<Self::WatchEventsStream>, Status> {
        let after = request.into_inner().after_event_id;
        if !after.is_empty() {
            validate_id(&after, "after_event_id")?;
        }
        let mut state = self.state.delivery.lock().await;
        state.subscribers.retain(|sender| !sender.is_closed());
        if !after.is_empty() && !state.retained.iter().any(|event| event.event_id == after) {
            return Err(Status::out_of_range("event cursor is outside retention"));
        }
        let backlog: Vec<_> = state
            .retained
            .iter()
            .skip_while(|event| !after.is_empty() && event.event_id != after)
            .skip(usize::from(!after.is_empty()))
            .cloned()
            .collect();
        let (sender, receiver) = mpsc::channel(self.state.retention);
        for event in backlog {
            sender
                .try_send(Ok(event))
                .map_err(|_| Status::internal("retained event queue overflow"))?;
        }
        if state.subscribers.len() == self.state.retention {
            return Err(Status::resource_exhausted(
                "event subscriber capacity reached",
            ));
        }
        state.subscribers.push(sender);
        drop(state);
        Ok(Response::new(Box::pin(ReceiverStream::new(receiver))))
    }
}

fn validate_id(value: &[u8], name: &'static str) -> Result<Vec<u8>, Status> {
    let id = Uuid::from_slice(value)
        .map_err(|_| Status::invalid_argument(format!("{name} must be a 16-byte UUID")))?;
    if id.get_version_num() != 7 {
        return Err(Status::invalid_argument(format!("{name} must be UUIDv7")));
    }
    Ok(value.to_vec())
}

fn validate_traceparent(value: &str) -> Result<(), Status> {
    let parts: Vec<_> = value.split('-').collect();
    if parts.len() != 4
        || parts[0] != "00"
        || parts[1].len() != 32
        || parts[2].len() != 16
        || parts[3].len() != 2
        || !parts.iter().all(|part| {
            part.bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        })
    {
        return Err(Status::invalid_argument("invalid traceparent"));
    }
    if parts[1] == "00000000000000000000000000000000" || parts[2] == "0000000000000000" {
        return Err(Status::invalid_argument("invalid traceparent"));
    }
    Ok(())
}

fn validate_command_times(
    issued_at: Option<&prost_types::Timestamp>,
    expires_at: Option<&prost_types::Timestamp>,
) -> Result<(), Status> {
    let issued_at = timestamp_duration(
        issued_at.ok_or_else(|| Status::invalid_argument("issued_at is required"))?,
        "issued_at",
    )?;
    let expires_at = timestamp_duration(
        expires_at.ok_or_else(|| Status::invalid_argument("expires_at is required"))?,
        "expires_at",
    )?;
    if expires_at <= issued_at {
        return Err(Status::invalid_argument("expires_at must follow issued_at"));
    }
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| Status::internal("system time precedes the Unix epoch"))?;
    if now >= expires_at {
        return Err(Status::deadline_exceeded("command envelope expired"));
    }
    Ok(())
}

fn timestamp_duration(
    timestamp: &prost_types::Timestamp,
    name: &'static str,
) -> Result<Duration, Status> {
    if timestamp.seconds < 0 || !(0..1_000_000_000).contains(&timestamp.nanos) {
        return Err(Status::invalid_argument(format!("invalid {name}")));
    }
    Ok(Duration::new(
        timestamp.seconds.cast_unsigned(),
        timestamp.nanos.cast_unsigned(),
    ))
}

fn new_event(
    node_id: &[u8],
    event_type: TransportEventType,
    traceparent: &str,
    payload: Vec<u8>,
) -> TransportEvent {
    TransportEvent {
        event_id: Uuid::now_v7().as_bytes().to_vec(),
        node_id: node_id.to_vec(),
        r#type: event_type.into(),
        occurred_at: Some(now_timestamp()),
        payload,
        traceparent: traceparent.to_owned(),
        endpoint_id: simulator_endpoint(node_id),
        connection_id: Vec::new(),
        owner_epoch: 0,
    }
}

fn simulator_endpoint(node_id: &[u8]) -> Vec<u8> {
    let mut digest = Sha256::new();
    digest.update(b"ocservia/development-simulator/");
    digest.update(node_id);
    digest.finalize().to_vec()
}

fn now_timestamp() -> prost_types::Timestamp {
    SystemTime::now().into()
}

#[cfg(test)]
fn new_traceparent() -> String {
    let trace_id = Uuid::now_v7().simple().to_string();
    let span_id = &Uuid::now_v7().simple().to_string()[..16];
    format!("00-{trace_id}-{span_id}-01")
}

#[cfg(test)]
mod tests {
    use super::*;
    use ocservia_contracts::generated::ocserv::platform::agent::v1::{
        CommandEnvelope, SemanticPayloadHashVersion,
    };
    use tokio_stream::StreamExt;

    fn probe_request(node_id: Vec<u8>, idempotency_key: Vec<u8>) -> SendCommandRequest {
        probe_request_with(
            node_id,
            idempotency_key,
            SimulationProbe {
                heartbeat_count: 1,
                delay_millis: 0,
                duplicate_event: false,
                return_error: false,
                disconnect_after: false,
            },
        )
    }

    fn probe_request_with(
        node_id: Vec<u8>,
        idempotency_key: Vec<u8>,
        probe: SimulationProbe,
    ) -> SendCommandRequest {
        let now = SystemTime::now();
        let envelope = CommandEnvelope {
            protocol_version: "1.0".to_owned(),
            message_id: Uuid::now_v7().as_bytes().to_vec(),
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key,
            node_id: node_id.clone(),
            sequence: 1,
            issued_at: Some(now.into()),
            expires_at: Some((now + Duration::from_mins(1)).into()),
            expected_revision: 0,
            traceparent: new_traceparent(),
            actor_id: "test".to_owned(),
            reason: "test".to_owned(),
            delivery_mode: CommandDeliveryMode::ExecuteOrReplay.into(),
            payload: Some(command_envelope::Payload::SimulationProbe(probe)),
            semantic_payload_hash_version: SemanticPayloadHashVersion::Unspecified as i32,
            semantic_payload_sha256: Vec::new(),
            ..CommandEnvelope::default()
        };
        SendCommandRequest {
            node_id,
            command_envelope: envelope.encode_to_vec(),
        }
    }

    #[test]
    fn traceparent_validation_is_strict() {
        assert!(validate_traceparent(&new_traceparent()).is_ok());
        assert!(validate_traceparent("not-a-trace").is_err());
        assert!(
            validate_traceparent("00-0123456789ABCDEF0123456789abcdef-0123456789abcdef-01")
                .is_err()
        );
    }

    #[tokio::test]
    async fn accepts_thirty_second_heartbeat_interval() {
        let service = StubService::new(8);
        service
            .send_command(Request::new(probe_request_with(
                Uuid::now_v7().as_bytes().to_vec(),
                Uuid::now_v7().as_bytes().to_vec(),
                SimulationProbe {
                    heartbeat_count: 1,
                    delay_millis: 30_000,
                    duplicate_event: false,
                    return_error: false,
                    disconnect_after: false,
                },
            )))
            .await
            .expect("30-second heartbeat accepted");
    }

    #[tokio::test]
    async fn expired_command_is_rejected_before_acceptance() {
        let service = StubService::new(8);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        let mut request = probe_request(node_id, Uuid::now_v7().as_bytes().to_vec());
        let mut envelope = CommandEnvelope::decode(request.command_envelope.as_slice())
            .expect("valid command envelope");
        let now = SystemTime::now();
        envelope.issued_at = Some((now - Duration::from_mins(2)).into());
        envelope.expires_at = Some((now - Duration::from_mins(1)).into());
        request.command_envelope = envelope.encode_to_vec();

        let error = service
            .send_command(Request::new(request))
            .await
            .expect_err("expired command rejected");
        assert_eq!(error.code(), tonic::Code::DeadlineExceeded);
        assert!(service.state.accepted_commands.lock().await.keys.is_empty());
    }

    #[test]
    fn queue_capacity_is_bounded() {
        let service = StubService::new(usize::MAX);
        assert_eq!(service.state.retention, 4096);
    }

    #[tokio::test]
    async fn capacity_mode_reports_bounds_and_emits_telemetry() {
        let service = StubService::new_capacity(8);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        service
            .send_command(Request::new(probe_request(
                node_id,
                Uuid::now_v7().as_bytes().to_vec(),
            )))
            .await
            .expect("capacity probe accepted");
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let stats = service.stats().await;
                if stats.active_tasks == 0 && stats.accepted_commands == 1 {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("capacity probe completed");

        let events = service.state.delivery.lock().await.retained.clone();
        assert!(events.iter().any(|event| {
            event.r#type == i32::from(TransportEventType::Telemetry)
                && TelemetryBatch::decode(event.payload.as_slice()).is_ok()
        }));
        assert_eq!(service.stats().await.task_capacity, 8);
    }

    #[test]
    fn maximum_payload_fits_the_transport_envelope_limit() {
        let event = new_event(
            Uuid::now_v7().as_bytes(),
            TransportEventType::Disconnected,
            &new_traceparent(),
            vec![0; MAX_COMMAND_BYTES],
        );
        assert!(event.encoded_len() <= MAX_COMMAND_BYTES + 4 * 1024);
    }

    #[tokio::test]
    async fn repeated_idempotency_key_runs_probe_once() {
        let service = StubService::new(8);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        let key = Uuid::now_v7().as_bytes().to_vec();

        for _ in 0..2 {
            let response = service
                .send_command(Request::new(probe_request(node_id.clone(), key.clone())))
                .await
                .expect("probe accepted");
            assert!(response.into_inner().accepted);
        }
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if service.state.delivery.lock().await.retained.len() == 3 {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("probe completed");

        assert_eq!(service.state.delivery.lock().await.retained.len(), 3);
    }

    #[tokio::test]
    async fn idempotency_keys_are_not_evicted_at_capacity() {
        let service = StubService::new(1);
        let first_node = Uuid::now_v7().as_bytes().to_vec();
        let first_key = Uuid::now_v7().as_bytes().to_vec();
        service
            .send_command(Request::new(probe_request(
                first_node.clone(),
                first_key.clone(),
            )))
            .await
            .expect("first probe accepted");
        tokio::time::timeout(Duration::from_secs(1), async {
            while service.state.tasks.available_permits() == 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("first probe completed");

        let second = service
            .send_command(Request::new(probe_request(
                Uuid::now_v7().as_bytes().to_vec(),
                Uuid::now_v7().as_bytes().to_vec(),
            )))
            .await
            .expect_err("new key rejected at capacity");
        assert_eq!(second.code(), tonic::Code::ResourceExhausted);
        service
            .send_command(Request::new(probe_request(first_node, first_key.clone())))
            .await
            .expect("original key remains idempotent");
        assert!(
            service
                .state
                .accepted_commands
                .lock()
                .await
                .keys
                .contains(&first_key)
        );
    }

    #[tokio::test]
    async fn closing_node_cancels_in_flight_probe_events() {
        let service = StubService::new(8);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        let request = probe_request_with(
            node_id.clone(),
            Uuid::now_v7().as_bytes().to_vec(),
            SimulationProbe {
                heartbeat_count: 2,
                delay_millis: 250,
                duplicate_event: false,
                return_error: false,
                disconnect_after: false,
            },
        );
        let traceparent = CommandEnvelope::decode(request.command_envelope.as_slice())
            .expect("valid command envelope")
            .traceparent;
        service
            .send_command(Request::new(request))
            .await
            .expect("probe accepted");
        tokio::time::timeout(Duration::from_secs(1), async {
            while !service.state.nodes.lock().await.contains_key(&node_id) {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("node connected");

        service
            .close_node(Request::new(CloseNodeRequest {
                node_id: node_id.clone(),
                reason: "operator close".to_owned(),
                fence_binding: None,
            }))
            .await
            .expect("connected node closed");
        tokio::time::sleep(Duration::from_millis(300)).await;

        let events = service.state.delivery.lock().await.retained.clone();
        assert_eq!(
            events
                .iter()
                .filter(|event| event.r#type == i32::from(TransportEventType::Disconnected))
                .count(),
            1
        );
        assert!(events.iter().all(|event| {
            event.r#type == i32::from(TransportEventType::Connected)
                || event.r#type == i32::from(TransportEventType::Disconnected)
        }));
        assert_eq!(
            events
                .iter()
                .find(|event| event.r#type == i32::from(TransportEventType::Disconnected))
                .expect("disconnect published")
                .traceparent,
            traceparent
        );
    }

    #[tokio::test]
    async fn slow_subscriber_does_not_block_node_close() {
        let service = StubService::new(1);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        let _stream = service
            .watch_events(Request::new(WatchEventsRequest::default()))
            .await
            .expect("watch accepted")
            .into_inner();
        service
            .send_command(Request::new(probe_request_with(
                node_id.clone(),
                Uuid::now_v7().as_bytes().to_vec(),
                SimulationProbe {
                    heartbeat_count: 2,
                    delay_millis: 0,
                    duplicate_event: false,
                    return_error: false,
                    disconnect_after: false,
                },
            )))
            .await
            .expect("probe accepted");
        tokio::time::timeout(Duration::from_secs(1), async {
            while !service.state.nodes.lock().await.contains_key(&node_id) {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("node connected");

        tokio::time::timeout(
            Duration::from_secs(1),
            service.close_node(Request::new(CloseNodeRequest {
                node_id,
                reason: "operator close".to_owned(),
                fence_binding: None,
            })),
        )
        .await
        .expect("close was not blocked by subscriber")
        .expect("node closed");
    }

    #[tokio::test]
    async fn cancellation_disconnects_subscribers_skipped_during_fanout() {
        let service = StubService::new(2);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        let generation = Uuid::now_v7();
        let (cancellation, _) = watch::channel(false);
        service.state.nodes.lock().await.insert(
            node_id.clone(),
            ActiveNode {
                connection: NodeConnection {
                    node_id: node_id.clone(),
                    endpoint_id: Uuid::now_v7().as_bytes().to_vec(),
                    path: ConnectionPath::Direct.into(),
                    round_trip_time_millis: 0,
                    connected_at: Some(now_timestamp()),
                    agent_instance_id: Uuid::now_v7().as_bytes().to_vec(),
                    path_detail: "stub/direct".to_owned(),
                    last_seen: Some(now_timestamp()),
                    negotiated_capabilities: Vec::new(),
                    authorization_revision: 0,
                    session_expires_at: None,
                    owner_epoch: 0,
                },
                generation,
                cancellation: cancellation.clone(),
                traceparent: new_traceparent(),
            },
        );
        let _slow = service
            .watch_events(Request::new(WatchEventsRequest::default()))
            .await
            .expect("slow watch accepted")
            .into_inner();
        let mut healthy = service
            .watch_events(Request::new(WatchEventsRequest::default()))
            .await
            .expect("healthy watch accepted")
            .into_inner();
        for event_type in [TransportEventType::Connected, TransportEventType::Heartbeat] {
            service
                .publish(new_event(
                    &node_id,
                    event_type,
                    &new_traceparent(),
                    Vec::new(),
                ))
                .await;
        }
        assert!(healthy.next().await.is_some());
        assert!(healthy.next().await.is_some());

        let publisher = tokio::spawn({
            let service = service.clone();
            let node_id = node_id.clone();
            async move {
                service
                    .publish_if_active(
                        &node_id,
                        generation,
                        new_event(
                            &node_id,
                            TransportEventType::CommandResult,
                            &new_traceparent(),
                            Vec::new(),
                        ),
                    )
                    .await
            }
        });
        tokio::task::yield_now().await;
        assert!(!publisher.is_finished());
        cancellation.send_replace(true);
        assert!(
            !tokio::time::timeout(Duration::from_secs(1), publisher)
                .await
                .expect("cancelled publication completed")
                .expect("publisher task completed")
        );
        assert!(service.state.delivery.lock().await.subscribers.is_empty());
        assert!(healthy.next().await.is_none());
    }

    #[tokio::test]
    async fn oversized_close_reason_preserves_connection() {
        let service = StubService::new(8);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        service
            .send_command(Request::new(probe_request_with(
                node_id.clone(),
                Uuid::now_v7().as_bytes().to_vec(),
                SimulationProbe {
                    heartbeat_count: 1,
                    delay_millis: 250,
                    duplicate_event: false,
                    return_error: false,
                    disconnect_after: false,
                },
            )))
            .await
            .expect("probe accepted");
        tokio::time::timeout(Duration::from_secs(1), async {
            while !service.state.nodes.lock().await.contains_key(&node_id) {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("node connected");

        let result = service
            .close_node(Request::new(CloseNodeRequest {
                node_id: node_id.clone(),
                reason: "x".repeat(MAX_COMMAND_BYTES + 1),
                fence_binding: None,
            }))
            .await
            .expect_err("oversized close rejected");
        assert_eq!(result.code(), tonic::Code::ResourceExhausted);
        assert!(service.state.nodes.lock().await.contains_key(&node_id));
        service
            .close_node(Request::new(CloseNodeRequest {
                node_id,
                reason: "cleanup".to_owned(),
                fence_binding: None,
            }))
            .await
            .expect("valid close accepted");
    }

    #[tokio::test]
    async fn closing_unknown_node_does_not_publish() {
        let service = StubService::new(8);
        let result = service
            .close_node(Request::new(CloseNodeRequest {
                node_id: Uuid::now_v7().as_bytes().to_vec(),
                reason: "test".to_owned(),
                fence_binding: None,
            }))
            .await;

        assert_eq!(
            result.expect_err("unknown node rejected").code(),
            tonic::Code::NotFound,
        );
        assert!(service.state.delivery.lock().await.retained.is_empty());
    }

    #[tokio::test]
    async fn slow_subscriber_applies_publication_backpressure() {
        let service = StubService::new(1);
        let node_id = Uuid::now_v7().as_bytes().to_vec();
        let mut stream = service
            .watch_events(Request::new(WatchEventsRequest::default()))
            .await
            .expect("watch accepted")
            .into_inner();
        service
            .publish(new_event(
                &node_id,
                TransportEventType::Connected,
                &new_traceparent(),
                Vec::new(),
            ))
            .await;
        let publisher = tokio::spawn({
            let service = service.clone();
            let node_id = node_id.clone();
            async move {
                service
                    .publish(new_event(
                        &node_id,
                        TransportEventType::Heartbeat,
                        &new_traceparent(),
                        Vec::new(),
                    ))
                    .await;
            }
        });
        tokio::task::yield_now().await;
        assert!(!publisher.is_finished());
        assert!(stream.next().await.is_some());
        tokio::time::timeout(Duration::from_secs(1), publisher)
            .await
            .expect("publisher unblocked")
            .expect("publisher task completed");
    }

    #[tokio::test]
    async fn closed_subscribers_do_not_exhaust_capacity() {
        let service = StubService::new(1);
        let first = service
            .watch_events(Request::new(WatchEventsRequest::default()))
            .await
            .expect("first watch accepted");
        drop(first);

        service
            .watch_events(Request::new(WatchEventsRequest::default()))
            .await
            .expect("closed subscriber reaped");
    }
}
