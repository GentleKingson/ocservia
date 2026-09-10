package mysql

func DispatchTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	steps := TimeColumnSteps("node_command_leases", []TimeColumn{{Name: "leased_until"}, {Name: "created_at"}}, "HEX(source.node_id)",
		[]string{"DROP INDEX node_command_leases_expiry_idx"}, []string{"ADD KEY node_command_leases_expiry_idx(leased_until)"})
	return append(steps, TimeColumnSteps("command_attempts", []TimeColumn{{Name: "started_at"}, {Name: "finished_at", Nullable: true}}, "HEX(source.id)",
		[]string{drop + "command_attempts_command_attempts_check"},
		[]string{"ADD CONSTRAINT command_attempts_command_attempts_check CHECK ((state='sending')=(finished_at IS NULL))"})...)
}
