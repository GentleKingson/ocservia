package mysql

func UserOperationsTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	var steps []LongKeyStep
	steps = append(steps, TimeColumnSteps("desired_user_policies", []TimeColumn{{Name: "expires_at", Nullable: true}, {Name: "created_at"}, {Name: "updated_at"}}, "CONCAT(HEX(source.node_id),':',HEX(source.username))", nil, nil)...)
	steps = append(steps, TimeColumnSteps("user_policy_mutations", []TimeColumn{{Name: "created_at"}}, "HEX(source.id)", nil, nil)...)
	steps = append(steps, TimeColumnSteps("batch_operations", []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}}, "HEX(source.id)", []string{"DROP INDEX batch_operations_active_idx"}, []string{"ADD KEY batch_operations_active_idx(updated_at,id)"})...)
	steps = append(steps, TimeColumnSteps("batch_operation_items", []TimeColumn{{Name: "lease_until", Nullable: true}, {Name: "updated_at"}}, "CONCAT(HEX(source.batch_id),':',source.item_index)", []string{"DROP INDEX batch_operation_items_claim_idx", drop + "batch_operation_items_batch_operation_items_check"}, []string{"ADD CONSTRAINT batch_operation_items_batch_operation_items_check CHECK((lease_owner IS NULL)=(lease_until IS NULL))", "ADD KEY batch_operation_items_claim_idx(updated_at,batch_id,item_index)"})...)
	steps = append(steps, TimeColumnSteps("scheduler_leases", []TimeColumn{{Name: "lease_until"}, {Name: "updated_at"}}, "HEX(source.lease_name)", nil, nil)...)
	// The exact natural key contains period_start. Its private registry must
	// match the source before conversion and the new integer encoding after it.
	key := longKeyBytes([]string{"node_id", "username", "policy_version", "cause", "period_start"}, "source.")
	verify := "SELECT IF(NOT EXISTS(SELECT 1 FROM user_policy_enforcements source LEFT JOIN exact_user_policy_enforcements registry ON registry.owner_id=source.exact_row_id WHERE registry.owner_id IS NULL OR registry.key_value<>" + key + "),'valid','invalid')"
	enforcement := timeColumnSteps("user_policy_enforcements", []TimeColumn{{Name: "period_start"}, {Name: "created_at"}}, "source.exact_row_id", nil, nil)
	enforcement[0].CheckBeforeSQL = verify
	enforcement = append(enforcement, LongKeyStep{Name: "user_policy_enforcements_logical_keys", Kind: "data", Object: "exact_user_policy_enforcements", SQL: "UPDATE exact_user_policy_enforcements registry JOIN user_policy_enforcements source ON source.exact_row_id=registry.owner_id SET registry.key_value=" + key, VerifySQL: verify, Repairable: true})
	steps = append(steps, GuardTimeSteps(enforcement, "user_policy_enforcements")...)
	return steps
}
