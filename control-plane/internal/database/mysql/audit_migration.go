package mysql

import (
	"fmt"
	"strings"
)

// AuditMigrationSteps changes storage, never the signed logical payload or
// published migration history. Guard triggers exclude writers before the
// append-only UPDATE triggers are temporarily removed for verified copying.
func AuditMigrationSteps() []LongKeyStep {
	validator := strings.Replace(jsonbObjectValidator, "ocserv_jsonb_object_valid", "ocserv_jsonb_value_valid", 1)
	validator = strings.Replace(validator, "RETURN CAST(JSON_TYPE(masked) AS BINARY)=_binary'OBJECT';", "RETURN TRUE;", 1)
	steps := []LongKeyStep{{Name: "jsonb_value_validator", Kind: "function", Object: "ocserv_jsonb_value_valid", SQL: validator}}
	var changes []LongKeyStep
	for _, table := range []string{"audit_events", "audit_checkpoints"} {
		name := table + "_reject_update"
		changes = append(changes, LongKeyStep{Name: "audit_drop_" + name, Kind: "trigger", Object: name, SQL: "DROP TRIGGER " + name})
		column := "occurred_at"
		var drops, adds []string
		if table == "audit_checkpoints" {
			column = "created_at"
		} else {
			drops = []string{"DROP INDEX audit_events_workspace_time_idx"}
			adds = []string{"ADD KEY audit_events_workspace_time_idx(workspace_id,occurred_at DESC,id DESC)"}
		}
		times := TimeColumnSteps(table, []TimeColumn{{Name: column}}, "HEX(source.id)", drops, adds)
		changes = append(changes, times[3:len(times)-3]...)
	}
	for _, column := range []string{"before_summary", "after_summary"} {
		shadow := "logical_" + column
		check := fmt.Sprintf("SELECT IF(NOT EXISTS(SELECT 1 FROM audit_events WHERE NOT (`%s` <=> CAST(`%s` AS BINARY)) OR (`%s` IS NOT NULL AND NOT ocserv_jsonb_value_valid(`%s`))),'valid','invalid')", shadow, column, shadow, shadow)
		changes = append(changes,
			LongKeyStep{Name: "audit_" + column + "_column", Kind: "table", Object: "audit_events", SQL: "ALTER TABLE audit_events ADD COLUMN `" + shadow + "` LONGBLOB NULL"},
			LongKeyStep{Name: "audit_" + column + "_backfill", Kind: "data", Object: "audit_events", Repairable: true, SQL: "UPDATE audit_events SET `" + shadow + "`=CAST(`" + column + "` AS BINARY)", VerifySQL: check},
			LongKeyStep{Name: "audit_" + column + "_switch", Kind: "table", Object: "audit_events", CheckBeforeSQL: check, SQL: "ALTER TABLE audit_events DROP COLUMN `" + column + "`,CHANGE COLUMN `" + shadow + "` `" + column + "` LONGBLOB NULL"},
		)
	}
	for _, table := range []string{"audit_events", "audit_checkpoints"} {
		name := table + "_reject_update"
		changes = append(changes, LongKeyStep{Name: "audit_restore_" + name, Kind: "trigger", Object: name, SQL: "CREATE TRIGGER " + name + " BEFORE UPDATE ON " + table + " FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='append-only audit record'"})
	}
	changes = append(changes, LongKeyStep{Name: "audit_jsonb_insert", Kind: "trigger", Object: "audit_jsonb_insert", SQL: `CREATE TRIGGER audit_jsonb_insert BEFORE INSERT ON audit_events FOR EACH ROW BEGIN IF (NEW.before_summary IS NOT NULL AND NOT ocserv_jsonb_value_valid(NEW.before_summary)) OR (NEW.after_summary IS NOT NULL AND NOT ocserv_jsonb_value_valid(NEW.after_summary)) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid audit JSON'; END IF; END`})
	return append(steps, GuardTimeSteps(changes, "audit_events", "audit_checkpoints")...)
}
