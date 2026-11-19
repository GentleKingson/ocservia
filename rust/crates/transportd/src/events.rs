use std::collections::VecDeque;
use std::sync::Arc;

use ocservia_contracts::generated::ocserv::platform::transport::v1::{
    TransportEvent, TransportEventType,
};
use tokio::sync::mpsc;
use tonic::Status;

use crate::{MAX_RETAINED_TELEMETRY_BYTES, Shared};

pub(crate) struct EventState {
    pub(crate) retained: VecDeque<Arc<TransportEvent>>,
    pub(crate) subscribers: Vec<mpsc::Sender<Result<Arc<TransportEvent>, Status>>>,
    pub(crate) telemetry_retained: usize,
    pub(crate) telemetry_retained_bytes: usize,
}

fn is_telemetry(event: &TransportEvent) -> bool {
    event.r#type == i32::from(TransportEventType::Telemetry)
}

impl EventState {
    pub(crate) fn new(capacity: usize) -> Self {
        Self {
            retained: VecDeque::with_capacity(capacity),
            subscribers: Vec::new(),
            telemetry_retained: 0,
            telemetry_retained_bytes: 0,
        }
    }

    /// Drops the oldest retained telemetry event; control events are never
    /// chosen. Returns whether one was removed.
    fn evict_oldest_telemetry(&mut self) -> bool {
        let Some(index) = self
            .retained
            .iter()
            .position(|event| is_telemetry(event.as_ref()))
        else {
            return false;
        };
        if let Some(evicted) = self.retained.remove(index) {
            self.telemetry_retained -= 1;
            self.telemetry_retained_bytes -= evicted.payload.len();
        }
        true
    }

    fn pop_oldest(&mut self) {
        if let Some(evicted) = self.retained.pop_front()
            && is_telemetry(&evicted)
        {
            self.telemetry_retained -= 1;
            self.telemetry_retained_bytes -= evicted.payload.len();
        }
    }
}

impl Shared {
    pub(super) async fn publish(&self, event: TransportEvent) {
        self.publish_event(event, false).await;
    }

    /// Publishes one node telemetry frame. `security` frames carry evidence and
    /// are delivered with the full subscriber queue; routine frames leave half
    /// of it for control events.
    pub(super) async fn publish_telemetry(&self, event: TransportEvent, security: bool) {
        self.publish_event(event, !security).await;
    }

    async fn publish_event(&self, event: TransportEvent, routine_telemetry: bool) {
        if *self.inner.shutdown.borrow() {
            return;
        }
        let telemetry = is_telemetry(&event);
        let payload_len = event.payload.len();
        let event = Arc::new(event);
        let capacity = self.inner.event_capacity;
        let mut state = self.inner.events.lock().await;
        let retainable = if telemetry {
            while state.telemetry_retained >= (capacity / 2).max(1)
                || state.telemetry_retained_bytes + payload_len > MAX_RETAINED_TELEMETRY_BYTES
            {
                if !state.evict_oldest_telemetry() {
                    break;
                }
            }
            // A full queue of control events is never displaced by telemetry.
            state.retained.len() < capacity || state.evict_oldest_telemetry()
        } else {
            if state.retained.len() == capacity && !state.evict_oldest_telemetry() {
                state.pop_oldest();
            }
            true
        };
        if retainable {
            if telemetry {
                state.telemetry_retained += 1;
                state.telemetry_retained_bytes += payload_len;
            }
            state.retained.push_back(Arc::clone(&event));
        }
        let reserve = capacity / 2;
        state.subscribers.retain(|sender| {
            if routine_telemetry && sender.capacity() <= reserve {
                // Routine telemetry yields to control events for a slow
                // consumer instead of evicting it.
                return !sender.is_closed();
            }
            matches!(sender.try_send(Ok(Arc::clone(&event))), Ok(()))
        });
    }

    pub(super) async fn subscribe_events(
        &self,
        after: &[u8],
    ) -> Result<mpsc::Receiver<Result<Arc<TransportEvent>, Status>>, Status> {
        let mut state = self.inner.events.lock().await;
        state.subscribers.retain(|sender| !sender.is_closed());
        if !after.is_empty() && !state.retained.iter().any(|item| item.event_id == after) {
            return Err(Status::out_of_range("event cursor is outside retention"));
        }
        if state.subscribers.len() == self.inner.event_capacity {
            return Err(Status::resource_exhausted(
                "event subscriber capacity reached",
            ));
        }
        let (sender, receiver) = mpsc::channel(self.inner.event_capacity);
        let backlog = state
            .retained
            .iter()
            .skip_while(|item| !after.is_empty() && item.event_id != after)
            .skip(usize::from(!after.is_empty()));
        for item in backlog {
            sender
                .try_send(Ok(Arc::clone(item)))
                .map_err(|_| Status::internal("retained event queue overflow"))?;
        }
        state.subscribers.push(sender);
        Ok(receiver)
    }
}
