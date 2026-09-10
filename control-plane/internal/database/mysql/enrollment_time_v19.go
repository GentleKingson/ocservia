package mysql

func EnrollmentTokenTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	var steps []LongKeyStep
	for _, table := range []string{"enrollment_tokens", "node_bootstrap_tokens"} {
		checks := []string{
			"ADD CONSTRAINT " + table + "_" + table + "_check CHECK(expires_at>created_at)",
		}
		drops := []string{"DROP INDEX " + table + "_workspace_expiry_idx", drop + table + "_" + table + "_check", drop + table + "_" + table + "_check1"}
		if table == "node_bootstrap_tokens" {
			drops = append(drops, drop+table+"_"+table+"_check2")
			checks = append(checks, "ADD CONSTRAINT node_bootstrap_tokens_node_bootstrap_tokens_check1 CHECK((bound_endpoint_id IS NULL)=(consumed_at IS NULL))", "ADD CONSTRAINT node_bootstrap_tokens_node_bootstrap_tokens_check2 CHECK((consumed_at IS NULL)=(consumed_node_id IS NULL))")
		} else {
			checks = append(checks, "ADD CONSTRAINT enrollment_tokens_enrollment_tokens_check1 CHECK((consumed_at IS NULL)=(consumed_node_id IS NULL))")
		}
		checks = append(checks, "ADD KEY "+table+"_workspace_expiry_idx(workspace_id,expires_at DESC)")
		steps = append(steps, TimeColumnSteps(table, []TimeColumn{{Name: "expires_at"}, {Name: "consumed_at", Nullable: true}, {Name: "created_at"}}, "HEX(source.id)", drops, checks)...)
	}
	return steps
}
