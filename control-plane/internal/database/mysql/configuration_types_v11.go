package mysql

import (
	"fmt"
	"strings"
)

func ConfigurationTypeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	changes := timeColumnSteps("config_plans", []TimeColumn{{Name: "expires_at"}, {Name: "created_at"}}, "HEX(source.id)", []string{"DROP INDEX config_plans_node_created_idx"}, []string{"ADD KEY config_plans_node_created_idx(node_id,created_at DESC,id DESC)"})
	check := `SELECT IF(NOT EXISTS(SELECT 1 FROM config_plans WHERE NOT (logical_warnings <=> CAST(warnings AS BINARY)) OR NOT ocserv_jsonb_array_valid(logical_warnings)),'valid','invalid')`
	changes = append(changes,
		LongKeyStep{Name: "config_warnings_blob", Kind: "table", Object: "config_plans", SQL: `ALTER TABLE config_plans ADD COLUMN logical_warnings LONGBLOB NULL`},
		LongKeyStep{Name: "config_warnings_copy", Kind: "data", Object: "config_plans", Repairable: true, SQL: `UPDATE config_plans SET logical_warnings=CAST(warnings AS BINARY)`, VerifySQL: check},
		LongKeyStep{Name: "config_warnings_switch", Kind: "table", Object: "config_plans", CheckBeforeSQL: check, SQL: `ALTER TABLE config_plans ` + drop + `config_plans_config_plans_warnings_check,DROP COLUMN warnings,CHANGE COLUMN logical_warnings warnings LONGBLOB NOT NULL DEFAULT ('[]')`},
	)
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := "config_plan_jsonb_" + strings.ToLower(event)
		changes = append(changes, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON config_plans FOR EACH ROW BEGIN IF NOT ocserv_jsonb_array_valid(NEW.warnings) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid configuration warnings JSON array'; END IF; END", name, event)})
	}
	steps := GuardTimeSteps(changes, "config_plans")
	steps = append(steps, TimeColumnSteps("config_apply_operations", []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}}, "HEX(source.operation_id)", []string{"DROP INDEX config_apply_operations_node_created_idx"}, []string{"ADD KEY config_apply_operations_node_created_idx(node_id,created_at DESC,operation_id DESC)"})...)
	return append(steps, TimeColumnSteps("node_config_state", []TimeColumn{{Name: "updated_at"}}, "HEX(source.node_id)", nil, nil)...)
}
