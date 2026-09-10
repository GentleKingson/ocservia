package mysql

import (
	"fmt"
	"strings"
)

// TelemetryWriteMigrationSteps extends the published history for the real
// ingestion writer. It does not change PostgreSQL migrations or claim that
// the remaining Controller read models have been ported.
func TelemetryWriteMigrationSteps(engine Engine) []LongKeyStep {
	clock := "(TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',CURRENT_TIMESTAMP(6)))"
	var steps []LongKeyStep
	steps = append(steps, TimeColumnSteps("telemetry_ingest_batches", []TimeColumn{{Name: "observed_at"}, {Name: "received_at", DefaultSQL: clock}}, "HEX(source.batch_id)", nil, nil)...)
	steps = append(steps, TimeColumnSteps("node_sessions", []TimeColumn{{Name: "connected_at"}, {Name: "observed_at"}}, "HEX(source.node_id)", []string{"DROP INDEX node_sessions_observed_idx"}, []string{"ADD KEY node_sessions_observed_idx(node_id,observed_at DESC,session_id)"})...)
	steps = append(steps, TimeColumnSteps("node_ip_bans", []TimeColumn{{Name: "observed_at"}}, "HEX(source.node_id)", nil, nil)...)
	steps = append(steps, TimeColumnSteps("observed_users", []TimeColumn{{Name: "observed_at"}}, "HEX(source.node_id)", nil, nil)...)
	steps = append(steps, TimeColumnSteps("node_agent_upgrade_results", []TimeColumn{{Name: "completed_at"}, {Name: "reported_at"}}, "HEX(source.operation_id)", nil, nil)...)
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	jsonSteps := timeColumnSteps("node_observed_snapshots", []TimeColumn{{Name: "observed_at"}, {Name: "received_at", DefaultSQL: clock}, {Name: "last_heartbeat_at"}}, "HEX(source.node_id)", []string{"DROP INDEX node_observed_freshness_idx"}, []string{"ADD KEY node_observed_freshness_idx(last_heartbeat_at)"})
	for _, column := range []string{"ocserv", "system", "path"} {
		table := "node_observed_snapshots"
		shadow := "logical_" + column
		check := fmt.Sprintf("SELECT IF(NOT EXISTS(SELECT 1 FROM %s WHERE NOT (`%s` <=> CAST(`%s` AS BINARY)) OR NOT ocserv_jsonb_object_valid(`%s`)),'valid','invalid')", table, shadow, column, shadow)
		constraint := "node_observed_snapshots_node_observed_snapshots_" + column + "_check"
		jsonSteps = append(jsonSteps,
			LongKeyStep{Name: table + "_" + column + "_blob", Kind: "table", Object: table, SQL: fmt.Sprintf("ALTER TABLE %s ADD COLUMN `%s` LONGBLOB NULL", table, shadow)},
			LongKeyStep{Name: table + "_" + column + "_copy", Kind: "data", Object: table, Repairable: true, SQL: fmt.Sprintf("UPDATE %s SET `%s`=CAST(`%s` AS BINARY)", table, shadow, column), VerifySQL: check},
			LongKeyStep{Name: table + "_" + column + "_switch", Kind: "table", Object: table, CheckBeforeSQL: check, SQL: fmt.Sprintf("ALTER TABLE %s %s`%s`,DROP COLUMN `%s`,CHANGE COLUMN `%s` `%s` LONGBLOB NOT NULL", table, drop, constraint, column, shadow, column)},
		)
	}
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := "node_snapshot_jsonb_" + strings.ToLower(event)
		jsonSteps = append(jsonSteps, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON node_observed_snapshots FOR EACH ROW BEGIN IF NOT ocserv_jsonb_object_valid(NEW.ocserv) OR NOT ocserv_jsonb_object_valid(NEW.system) OR NOT ocserv_jsonb_object_valid(NEW.path) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid snapshot JSON object'; END IF; END", name, event)})
	}
	return append(steps, GuardTimeSteps(jsonSteps, "node_observed_snapshots")...)
}
