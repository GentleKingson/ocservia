use super::{MAX_RETAINED_TELEMETRY_BYTES, Shared, Status, TransportEvent, is_telemetry, mpsc};
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
        let capacity = self.inner.event_capacity;
        let mut state = self.inner.events.lock().await;
        let retainable = if is_telemetry(&event) {
            let bytes = event.payload.len();
            while state.telemetry_retained >= (capacity / 2).max(1)
                || state.telemetry_retained_bytes + bytes > MAX_RETAINED_TELEMETRY_BYTES
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
            if is_telemetry(&event) {
                state.telemetry_retained += 1;
                state.telemetry_retained_bytes += event.payload.len();
            }
            state.retained.push_back(event.clone());
        }
        let reserve = capacity / 2;
        state.subscribers.retain(|sender| {
            if routine_telemetry && sender.capacity() <= reserve {
                // Routine telemetry yields to control events for a slow
                // consumer instead of evicting it.
                return !sender.is_closed();
            }
            matches!(sender.try_send(Ok(event.clone())), Ok(()))
        });
    }

    pub(super) async fn subscribe_events(
        &self,
        after: &[u8],
    ) -> Result<mpsc::Receiver<Result<TransportEvent, Status>>, Status> {
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
        let backlog = state
            .retained
            .iter()
            .skip_while(|item| !after.is_empty() && item.event_id != after)
            .skip(usize::from(!after.is_empty()))
            .cloned()
            .collect::<Vec<_>>();
        let (sender, receiver) = mpsc::channel(self.inner.event_capacity);
        for item in backlog {
            sender
                .try_send(Ok(item))
                .map_err(|_| Status::internal("retained event queue overflow"))?;
        }
        state.subscribers.push(sender);
        Ok(receiver)
    }
}
