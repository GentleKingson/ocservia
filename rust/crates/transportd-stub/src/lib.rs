//! Side-effect-free transport stub and bounded multi-node agent simulator.

#![forbid(unsafe_code)]

use std::collections::{HashMap, HashSet, VecDeque};
use std::pin::Pin;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use ocservia_contracts::generated::ocserv::platform::agent::v1::{
    CommandEnvelope, SimulationProbe, command_envelope,
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

const MAX_COMMAND_BYTES: usize = 1024 * 1024;
const MAX_HEARTBEATS: u32 = 32;
const MAX_DELAY: Duration = Duration::from_secs(10);

type EventStream = Pin<Box<dyn Stream<Item = Result<TransportEvent, Status>> + Send>>;

#[derive(Clone)]
pub struct StubService {
    state: Arc<State>,
}

struct State {
    delivery: Mutex<EventState>,
    nodes: Mutex<HashMap<Vec<u8>, ActiveNode>>,
    accepted_commands: Mutex<AcceptedCommands>,
    retention: usize,
    tasks: Arc<Semaphore>,
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
        let capacity = queue_capacity.clamp(1, 4096);
        Self {
            state: Arc::new(State {
                delivery: Mutex::new(EventState {
                    retained: VecDeque::with_capacity(capacity),
                    subscribers: Vec::new(),
                }),
                nodes: Mutex::new(HashMap::new()),
                accepted_commands: Mutex::new(AcceptedCommands {
                    keys: HashSet::with_capacity(capacity),
                }),
                retention: capacity,
                tasks: Arc::new(Semaphore::new(capacity)),
            }),
        }
    }

    async fn publish(&self, event: TransportEvent) {
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

    async fn run_probe(self, node_id: Vec<u8>, traceparent: String, probe: SimulationProbe) {
        let delay = Duration::from_millis(u64::from(probe.delay_millis));
        let connected_at = now_timestamp();
        let generation = Uuid::now_v7();
        let (cancellation, mut cancelled) = watch::channel(false);
        self.state.nodes.lock().await.insert(
            node_id.clone(),
            ActiveNode {
                connection: NodeConnection {
                    node_id: node_id.clone(),
                    endpoint_id: Uuid::now_v7().as_bytes().to_vec(),
                    path: ConnectionPath::Direct.into(),
                    round_trip_time_millis: probe.delay_millis.into(),
                    connected_at: Some(connected_at),
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
        }

        let (event_type, payload) = if probe.return_error {
            (TransportEventType::Error, b"simulated error".to_vec())
        } else {
            (TransportEventType::CommandResult, b"completed".to_vec())
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

#[tonic::async_trait]
impl TransportService for StubService {
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
        let envelope = CommandEnvelope::decode(request.command_envelope.as_slice())
            .map_err(|_| Status::invalid_argument("invalid command envelope"))?;
        if envelope.node_id != node_id {
            return Err(Status::invalid_argument(
                "command node_id does not match request",
            ));
        }
        validate_traceparent(&envelope.traceparent)?;
        let idempotency_key = validate_id(&envelope.idempotency_key, "idempotency_key")?;
        let Some(command_envelope::Payload::SimulationProbe(probe)) = envelope.payload else {
            return Err(Status::unimplemented("stub accepts only simulation probes"));
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
    }
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
        let envelope = CommandEnvelope {
            protocol_version: "1.0".to_owned(),
            message_id: Uuid::now_v7().as_bytes().to_vec(),
            command_id: Uuid::now_v7().as_bytes().to_vec(),
            idempotency_key,
            node_id: node_id.clone(),
            sequence: 1,
            issued_at: None,
            expires_at: None,
            expected_revision: 0,
            traceparent: new_traceparent(),
            actor_id: "test".to_owned(),
            reason: "test".to_owned(),
            payload: Some(command_envelope::Payload::SimulationProbe(probe)),
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

    #[test]
    fn queue_capacity_is_bounded() {
        let service = StubService::new(usize::MAX);
        assert_eq!(service.state.retention, 4096);
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
            })),
        )
        .await
        .expect("close was not blocked by subscriber")
        .expect("node closed");
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
            }))
            .await
            .expect_err("oversized close rejected");
        assert_eq!(result.code(), tonic::Code::ResourceExhausted);
        assert!(service.state.nodes.lock().await.contains_key(&node_id));
        service
            .close_node(Request::new(CloseNodeRequest {
                node_id,
                reason: "cleanup".to_owned(),
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
