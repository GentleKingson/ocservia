package mysql

func TransportTimeSteps() []LongKeyStep {
	steps := TimeColumnSteps("transport_events", []TimeColumn{{Name: "occurred_at"}, {Name: "received_at", DefaultSQL: "(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)))"}}, "HEX(source.event_id)", nil, nil)
	steps = append(steps, TimeColumnSteps("transport_event_quarantine", []TimeColumn{{Name: "observed_at"}}, "HEX(source.event_id)", []string{"DROP INDEX transport_event_quarantine_node_time_idx"}, []string{"ADD KEY transport_event_quarantine_node_time_idx(node_id,observed_at DESC)"})...)
	steps = append(steps, TimeColumnSteps("transport_event_cursor", []TimeColumn{{Name: "updated_at"}}, "source.singleton", nil, nil)...)
	return append(steps, TimeColumnSteps("local_slice_jobs", []TimeColumn{{Name: "available_at"}, {Name: "expires_at"}, {Name: "dispatched_at", Nullable: true}, {Name: "created_at"}}, "HEX(source.operation_id)", []string{"DROP INDEX local_slice_jobs_dispatch_idx"}, []string{"ADD KEY local_slice_jobs_dispatch_idx(available_at,operation_id)"})...)
}
