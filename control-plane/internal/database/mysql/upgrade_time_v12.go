package mysql

func UpgradeTimeSteps() []LongKeyStep {
	return TimeColumnSteps("agent_upgrade_operations", []TimeColumn{{Name: "scheduled_at", Nullable: true}, {Name: "completed_at", Nullable: true}, {Name: "created_at"}, {Name: "updated_at"}}, "HEX(source.operation_id)", nil, nil)
}
