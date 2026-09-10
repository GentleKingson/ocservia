package mysql

// Guard every affected business table before copying any data. DDL waits for
// existing writers; the guards survive a crashed migrator. Only the connection
// holding the migration lock can then change those tables. A repair rechecks
// each copy before consuming its source, including owner-edited legacy data.
func guardedRevisionSteps(changes []LongKeyStep) []LongKeyStep {
	// Batch deletion cascades into legacy samples without firing their
	// DELETE triggers, so its parent must be guarded as well.
	tables := []string{"telemetry_ingest_batches"}
	seen := map[string]bool{}
	for _, s := range changes {
		if s.Kind != "table" || seen[s.Object] {
			continue
		}
		// Only pre-existing business tables need write guards; new private
		// tables have no runtime grants and cannot have concurrent writers.
		switch s.Object {
		case "observed_groups", "telemetry_security_events", "telemetry_samples", "telemetry_rollups_5m", "telemetry_rollups_1h", "identities", "workspaces", "nodes", "operations", "agent_command_results", "upstream_sync_records", "user_policy_enforcements", "commands":
			tables = append(tables, s.Object)
			seen[s.Object] = true
		}
	}
	var result []LongKeyStep
	for _, table := range tables {
		for _, event := range []struct{ sql, suffix string }{{"INSERT", "insert"}, {"UPDATE", "update"}, {"DELETE", "delete"}} {
			name := "migrate_" + table + "_" + event.suffix
			body := "CREATE TRIGGER `" + name + "` BEFORE " + event.sql + " ON `" + table + "` FOR EACH ROW BEGIN DECLARE holder BIGINT; SET holder=IS_USED_LOCK(CONCAT('ocservia:',LEFT(SHA2(DATABASE(),256),48))); IF holder IS NULL OR holder<>CONNECTION_ID() THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='schema migration requires exclusive writer'; END IF; END"
			result = append(result, LongKeyStep{Name: name, SQL: body, Kind: "trigger", Object: name})
		}
	}
	result = append(result, changes...)
	for _, table := range tables {
		for _, event := range []string{"insert", "update", "delete"} {
			name := "migrate_" + table + "_" + event
			result = append(result, LongKeyStep{Name: "drop_" + name, SQL: "DROP TRIGGER `" + name + "`", Kind: "trigger", Object: name})
		}
	}
	return result
}
