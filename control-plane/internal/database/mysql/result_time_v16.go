package mysql

func ResultTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	return TimeColumnSteps("agent_command_results", []TimeColumn{{Name: "accepted_at", Nullable: true}, {Name: "completed_at"}, {Name: "created_at"}}, "HEX(source.event_id)",
		[]string{drop + "agent_command_results_agent_command_results_check", drop + "agent_command_results_agent_command_results_check1", "DROP INDEX agent_command_results_command_created_idx"},
		[]string{"ADD CONSTRAINT agent_command_results_agent_command_results_check CHECK (accepted_at IS NULL OR accepted_at<=completed_at)",
			"ADD CONSTRAINT agent_command_results_agent_command_results_check1 CHECK ((state='succeeded' AND payload_sha256 IS NOT NULL AND accepted_at IS NOT NULL AND error_code IS NULL) OR (state IN ('failed','unknown') AND payload_sha256 IS NOT NULL AND accepted_at IS NOT NULL AND error_code IS NOT NULL AND (state<>'unknown' OR OCTET_LENGTH(result)=0)) OR (state='rejected' AND accepted_at IS NULL AND error_code IS NOT NULL AND OCTET_LENGTH(result)=0))",
			"ADD KEY agent_command_results_command_created_idx(command_id,created_at)"})
}
