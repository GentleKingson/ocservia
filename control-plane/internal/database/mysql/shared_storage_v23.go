package mysql

import (
	"fmt"
	"strings"
)

func SharedStorageSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	steps := TimeColumnSteps("workspaces", []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}, {Name: "archived_at", Nullable: true}}, "HEX(source.id)", nil, nil)
	steps = append(steps, TimeColumnSteps("node_endpoint_keys", []TimeColumn{{Name: "bound_at"}, {Name: "revoked_at", Nullable: true}}, "HEX(source.node_id)",
		[]string{drop + "node_endpoint_keys_node_endpoint_keys_check"},
		[]string{`ADD CONSTRAINT node_endpoint_keys_node_endpoint_keys_check CHECK ((state='revoked')=(revoked_at IS NOT NULL))`})...)
	steps = append(steps, TimeColumnSteps("node_sealing_keys", []TimeColumn{{Name: "created_at"}}, "CONCAT(HEX(source.node_id),':',source.purpose)", nil, nil)...)
	for _, spec := range []struct {
		table, column, validator, constraint, defaultSQL string
		times                                            []TimeColumn
	}{
		{"nodes", "labels", "ocserv_jsonb_value_valid", "", " DEFAULT ('{}')", []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}}},
		{"upstream_sync_records", "classification", "ocserv_jsonb_object_valid", "upstream_sync_records_upstream_sync_records_classification_check", "", []TimeColumn{{Name: "synced_at"}}},
	} {
		changes := timeColumnSteps(spec.table, spec.times, "HEX(source.id)", nil, nil)
		shadow := "logical_" + spec.column
		check := fmt.Sprintf("SELECT IF(NOT EXISTS(SELECT 1 FROM `%s` WHERE NOT (`%s` <=> CAST(`%s` AS BINARY)) OR NOT %s(`%s`)),'valid','invalid')", spec.table, shadow, spec.column, spec.validator, shadow)
		dropConstraint := ""
		if spec.constraint != "" {
			dropConstraint = drop + spec.constraint + ","
		}
		changes = append(changes,
			LongKeyStep{Name: spec.table + "_" + spec.column + "_blob", Kind: "table", Object: spec.table, SQL: fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` LONGBLOB NULL", spec.table, shadow)},
			LongKeyStep{Name: spec.table + "_" + spec.column + "_copy", Kind: "data", Object: spec.table, Repairable: true, SQL: fmt.Sprintf("UPDATE `%s` SET `%s`=CAST(`%s` AS BINARY)", spec.table, shadow, spec.column), VerifySQL: check},
			LongKeyStep{Name: spec.table + "_" + spec.column + "_switch", Kind: "table", Object: spec.table, CheckBeforeSQL: check, SQL: fmt.Sprintf("ALTER TABLE `%s` %sDROP COLUMN `%s`,CHANGE COLUMN `%s` `%s` LONGBLOB NOT NULL%s", spec.table, dropConstraint, spec.column, shadow, spec.column, spec.defaultSQL)},
		)
		for _, event := range []string{"INSERT", "UPDATE"} {
			name := spec.table + "_jsonb_" + strings.ToLower(event)
			changes = append(changes, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON `%s` FOR EACH ROW BEGIN IF NOT %s(NEW.`%s`) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid shared storage JSON'; END IF; END", name, event, spec.table, spec.validator, spec.column)})
		}
		steps = append(steps, GuardTimeSteps(changes, spec.table)...)
	}
	return steps
}
