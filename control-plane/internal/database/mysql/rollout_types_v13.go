package mysql

import (
	"fmt"
	"strings"
)

func RolloutTypeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	validator := strings.Replace(jsonbObjectValidator, "ocserv_jsonb_object_valid", "ocserv_rollout_exclusions_valid", 1)
	validator = strings.Replace(validator, "RETURN CAST(JSON_TYPE(masked) AS BINARY)=_binary'OBJECT';", "RETURN CAST(JSON_TYPE(masked) AS BINARY)=_binary'ARRAY' AND JSON_LENGTH(masked)<=500;", 1)
	steps := []LongKeyStep{{Name: "rollout_exclusions_validator", Kind: "function", Object: "ocserv_rollout_exclusions_valid", SQL: validator}}
	changes := timeColumnSteps("agent_rollouts", []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}}, "HEX(source.id)", []string{"DROP INDEX agent_rollouts_active_idx", "DROP INDEX agent_rollouts_workspace_created_idx"}, []string{"ADD KEY agent_rollouts_active_idx(created_at)", "ADD KEY agent_rollouts_workspace_created_idx(workspace_id,created_at DESC)"})
	check := `SELECT IF(NOT EXISTS(SELECT 1 FROM agent_rollouts WHERE NOT (logical_exclusions <=> CAST(exclusions AS BINARY)) OR NOT ocserv_rollout_exclusions_valid(logical_exclusions)),'valid','invalid')`
	changes = append(changes,
		LongKeyStep{Name: "rollout_exclusions_blob", Kind: "table", Object: "agent_rollouts", SQL: `ALTER TABLE agent_rollouts ADD COLUMN logical_exclusions LONGBLOB NULL`},
		LongKeyStep{Name: "rollout_exclusions_copy", Kind: "data", Object: "agent_rollouts", Repairable: true, SQL: `UPDATE agent_rollouts SET logical_exclusions=CAST(exclusions AS BINARY)`, VerifySQL: check},
		LongKeyStep{Name: "rollout_exclusions_switch", Kind: "table", Object: "agent_rollouts", CheckBeforeSQL: check, SQL: `ALTER TABLE agent_rollouts ` + drop + `agent_rollouts_agent_rollouts_exclusions_check,DROP COLUMN exclusions,CHANGE COLUMN logical_exclusions exclusions LONGBLOB NOT NULL DEFAULT ('[]')`},
	)
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := "rollout_jsonb_" + strings.ToLower(event)
		changes = append(changes, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON agent_rollouts FOR EACH ROW BEGIN IF NOT ocserv_rollout_exclusions_valid(NEW.exclusions) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid rollout exclusions JSON array'; END IF; END", name, event)})
	}
	steps = append(steps, GuardTimeSteps(changes, "agent_rollouts")...)
	return append(steps, TimeColumnSteps("agent_rollout_nodes", []TimeColumn{{Name: "dispatch_lease_until", Nullable: true}, {Name: "updated_at"}}, "CONCAT(HEX(source.rollout_id),HEX(source.node_id))", nil, nil)...)
}
