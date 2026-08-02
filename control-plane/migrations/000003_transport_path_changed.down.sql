DELETE FROM transport_events WHERE event_type = 'path_changed';
ALTER TABLE transport_events
    DROP CONSTRAINT transport_events_event_type_check;
ALTER TABLE transport_events
    ADD CONSTRAINT transport_events_event_type_check
    CHECK (event_type IN ('connected', 'disconnected', 'command_result', 'heartbeat', 'error'));
