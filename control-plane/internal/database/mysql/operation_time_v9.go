package mysql

func OperationReadTimeSteps() []LongKeyStep {
	steps := TimeColumnSteps("operations", []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}, {Name: "expires_at", Nullable: true}, {Name: "completed_at", Nullable: true}}, "HEX(source.id)", []string{"DROP INDEX operations_workspace_created_idx"}, []string{"ADD KEY operations_workspace_created_idx(workspace_id,created_at DESC,id DESC)"})
	return append(steps, TimeColumnSteps("operation_events", []TimeColumn{{Name: "occurred_at"}}, "HEX(source.id)", nil, nil)...)
}
