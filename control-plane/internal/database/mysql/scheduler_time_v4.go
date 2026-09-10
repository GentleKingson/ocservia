package mysql

// SchedulerTimeSteps recognizes only the exact, published never-acquired
// singleton seed. An existing owner decision wins; non-seed year-1000 values
// require an operator decision while holding the migration connection lock.
func SchedulerTimeSteps() []LongKeyStep {
	seed := `id=1 AND instance_id=UNHEX('00000000000000000000000000000000') AND incarnation=0 AND epoch=0 AND lease_until='1000-01-01 00:00:00.000000'`
	decision := `d.table_name='scheduler_leadership' AND d.column_name='lease_until' AND d.row_key=CAST(source.id AS BINARY) AND d.source_value=source.lease_until`
	steps := []LongKeyStep{{Name: "scheduler_seed_time_decision", Kind: "data", Object: "time_migration_decisions", Repairable: true, SQL: `INSERT INTO time_migration_decisions(table_name,column_name,row_key,source_value,decision) SELECT 'scheduler_leadership','lease_until',CAST(source.id AS BINARY),source.lease_until,'negative_infinity' FROM scheduler_leadership source WHERE ` + seed + ` AND NOT EXISTS(SELECT 1 FROM time_migration_decisions d WHERE d.table_name='scheduler_leadership' AND d.column_name='lease_until' AND d.row_key=CAST(source.id AS BINARY))`, VerifySQL: `SELECT IF(NOT EXISTS(SELECT 1 FROM scheduler_leadership source WHERE ` + seed + ` AND NOT EXISTS(SELECT 1 FROM time_migration_decisions d WHERE ` + decision + `)),'valid','invalid')`}}
	steps = append(steps, timeColumnSteps("scheduler_leadership", []TimeColumn{{Name: "lease_until", LegacyInfinity: true}, {Name: "updated_at"}}, "source.id", nil, nil)...)
	return GuardTimeSteps(steps, "scheduler_leadership")
}
