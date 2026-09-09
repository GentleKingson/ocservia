package mysql

func RBACMigrationSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	steps := TimeColumnSteps("role_bindings", []TimeColumn{{Name: "created_at"}}, "HEX(source.id)", nil, nil)
	steps = append(steps, TimeColumnSteps("local_auth_bootstrap", []TimeColumn{{Name: "completed_at", Nullable: true}}, "source.singleton", []string{drop + "local_auth_bootstrap_local_initialization_state"}, []string{"ADD CONSTRAINT local_auth_bootstrap_local_initialization_state CHECK ((completion_pending AND completed_at IS NULL AND approver_identity_id IS NULL) OR (NOT completion_pending AND completed_at IS NOT NULL))"})...)
	steps = append(steps, TimeColumnSteps("approval_requests", []TimeColumn{{Name: "expires_at"}, {Name: "created_at"}, {Name: "consumed_at", Nullable: true}}, "HEX(source.id)", []string{
		"DROP INDEX approval_requests_scope_idx",
		drop + "approval_requests_approval_requests_check2",
		drop + "approval_requests_approval_requests_check3",
	}, []string{
		"ADD KEY approval_requests_scope_idx(workspace_id,resource_type,resource_id,action,status,expires_at)",
		"ADD CONSTRAINT approval_requests_approval_requests_check2 CHECK ((status='consumed')=(consumed_at IS NOT NULL))",
		"ADD CONSTRAINT approval_requests_approval_requests_check3 CHECK (expires_at>created_at)",
	})...)
	return steps
}
