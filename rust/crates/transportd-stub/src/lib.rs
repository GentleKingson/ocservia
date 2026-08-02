//! Side-effect-free transport stub and bounded multi-node agent simulator.

#![forbid(unsafe_code)]

use std::collections::{HashMap, VecDeque};
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
use tokio::sync::{Mutex, Semaphore, broadcast};
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
    retained: Mutex<VecDeque<TransportEvent>>,
    nodes: Mutex<HashMap<Vec<u8>, NodeConnection>>,
    events: broadcast::Sender<TransportEvent>,
    retention: usize,
    tasks: Arc<Semaphore>,
}

impl StubService {
    /// Creates a service with bounded replay and subscriber queues.
    #[must_use]
    pub fn new(queue_capacity: usize) -> Self {
        let capacity = queue_capacity.clamp(1, 4096);
        let (events, _) = broadcast::channel(capacity);
        Self {
            state: Arc::new(State {
                retained: Mutex::new(VecDeque::with_capacity(capacity)),
                nodes: Mutex::new(HashMap::new()),
                events,
                retention: capacity,
                tasks: Arc::new(Semaphore::new(capacity)),
            }),
        }
    }

    async fn publish(&self, event: TransportEvent) {
        let mut retained = self.state.retained.lock().await;
        if retained.len() == self.state.retention {
            retained.pop_front();
        }
        retained.push_back(event.clone());
        drop(retained);
        let _subscriber_count = self.state.events.send(event);
    }

    async fn run_probe(self, node_id: Vec<u8>, traceparent: String, probe: SimulationProbe) {
        let delay = Duration::from_millis(u64::from(probe.delay_millis));
        let connected_at = now_timestamp();
        self.state.nodes.lock().await.insert(
            node_id.clone(),
            NodeConnection {
                node_id: node_id.clone(),
                endpoint_id: Uuid::now_v7().as_bytes().to_vec(),
                path: ConnectionPath::Direct.into(),
                round_trip_time_millis: probe.delay_millis.into(),
                connected_at: Some(connected_at),
            },
        );
        self.publish(new_event(
            &node_id,
            TransportEventType::Connected,
            &traceparent,
            b"connected".to_vec(),
        ))
        .await;

        for sequence in 0..probe.heartbeat_count.max(1) {
            tokio::time::sleep(delay).await;
            let event = new_event(
                &node_id,
                TransportEventType::Heartbeat,
                &traceparent,
                format!("heartbeat:{sequence}").into_bytes(),
            );
            self.publish(event.clone()).await;
            if probe.duplicate_event {
                self.publish(event).await;
            }
        }

        let (event_type, payload) = if probe.return_error {
            (TransportEventType::Error, b"simulated error".to_vec())
        } else {
            (TransportEventType::CommandResult, b"completed".to_vec())
        };
        self.publish(new_event(&node_id, event_type, &traceparent, payload))
            .await;

        if probe.disconnect_after {
            self.state.nodes.lock().await.remove(&node_id);
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
            .cloned()
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
        let Some(command_envelope::Payload::SimulationProbe(probe)) = envelope.payload else {
            return Err(Status::unimplemented("stub accepts only simulation probes"));
        };
        if probe.heartbeat_count > MAX_HEARTBEATS
            || Duration::from_millis(u64::from(probe.delay_millis)) > MAX_DELAY
        {
            return Err(Status::invalid_argument("simulation limits exceeded"));
        }
        let permit = self
            .state
            .tasks
            .clone()
            .try_acquire_owned()
            .map_err(|_| Status::resource_exhausted("simulator task capacity reached"))?;
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
        self.state.nodes.lock().await.remove(&node_id);
        self.publish(new_event(
            &node_id,
            TransportEventType::Disconnected,
            &new_traceparent(),
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
        let retained = self.state.retained.lock().await;
        if !after.is_empty()
            && !retained.is_empty()
            && !retained.iter().any(|event| event.event_id == after)
        {
            return Err(Status::out_of_range("event cursor is outside retention"));
        }
        let backlog: Vec<_> = retained
            .iter()
            .skip_while(|event| !after.is_empty() && event.event_id != after)
            .skip(usize::from(!after.is_empty()))
            .cloned()
            .collect();
        let mut subscription = self.state.events.subscribe();
        let (sender, receiver) = tokio::sync::mpsc::channel(self.state.retention);
        tokio::spawn(async move {
            for event in backlog {
                if sender.send(Ok(event)).await.is_err() {
                    return;
                }
            }
            loop {
                match subscription.recv().await {
                    Ok(event) => {
                        if sender.send(Ok(event)).await.is_err() {
                            return;
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(_)) => {
                        let _result = sender
                            .send(Err(Status::resource_exhausted(
                                "event consumer fell behind",
                            )))
                            .await;
                        return;
                    }
                    Err(broadcast::error::RecvError::Closed) => return,
                }
            }
        });
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
        || !parts
            .iter()
            .all(|part| part.bytes().all(|byte| byte.is_ascii_hexdigit()))
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

fn new_traceparent() -> String {
    let trace_id = Uuid::now_v7().simple().to_string();
    let span_id = &Uuid::now_v7().simple().to_string()[..16];
    format!("00-{trace_id}-{span_id}-01")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn traceparent_validation_is_strict() {
        assert!(validate_traceparent(&new_traceparent()).is_ok());
        assert!(validate_traceparent("not-a-trace").is_err());
    }

    #[test]
    fn queue_capacity_is_bounded() {
        let service = StubService::new(usize::MAX);
        assert_eq!(service.state.retention, 4096);
    }
}
