DROP TABLE agent_command_results;

UPDATE commands SET state = 'failed' WHERE state = 'rejected';
ALTER TABLE commands
    DROP CONSTRAINT commands_state_check,
    ADD CONSTRAINT commands_state_check CHECK (
        state IN ('queued', 'dispatched', 'accepted', 'running', 'succeeded', 'failed', 'unknown', 'expired', 'rolled_back', 'superseded')
    );

DELETE FROM transport_events WHERE event_type = 'simulation_result';
ALTER TABLE transport_events
    DROP CONSTRAINT transport_events_event_type_check,
    ADD CONSTRAINT transport_events_event_type_check CHECK (
        event_type IN ('connected', 'disconnected', 'command_result', 'heartbeat', 'error', 'path_changed', 'telemetry')
    );
