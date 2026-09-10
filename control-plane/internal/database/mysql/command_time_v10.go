package mysql

func CommandTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	steps := TimeColumnSteps("commands", []TimeColumn{{Name: "expires_at"}, {Name: "created_at"}, {Name: "updated_at"}}, "HEX(source.id)",
		[]string{drop + "commands_commands_check", "DROP INDEX commands_node_state_idx", "DROP INDEX commands_pending_resource_idx"},
		[]string{"ADD CONSTRAINT commands_commands_check CHECK (expires_at>created_at)", "ADD KEY commands_node_state_idx(node_id,state,created_at,id)", "ADD KEY commands_pending_resource_idx(node_id,resource_type,created_at)"})
	return append(steps, TimeColumnSteps("outbox_events", []TimeColumn{{Name: "available_at"}, {Name: "locked_until", Nullable: true}, {Name: "published_at", Nullable: true}, {Name: "created_at"}}, "HEX(source.id)",
		[]string{drop + "outbox_events_outbox_events_check", drop + "outbox_events_outbox_events_check1", "DROP INDEX outbox_events_dispatch_idx"},
		[]string{"ADD CONSTRAINT outbox_events_outbox_events_check CHECK ((locked_by IS NULL)=(locked_until IS NULL))", "ADD CONSTRAINT outbox_events_outbox_events_check1 CHECK (published_at IS NULL OR locked_by IS NULL)", "ADD KEY outbox_events_dispatch_idx(available_at,id)"})...)
}
