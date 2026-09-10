package mysql

import (
	"fmt"
	"strings"
)

func DesiredStateTypeSteps(engine Engine) []LongKeyStep {
	var steps []LongKeyStep
	for _, table := range []string{"desired_users", "desired_groups"} {
		steps = append(steps, timeColumnSteps(table, []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}}, "CONCAT(HEX(source.node_id),':',HEX(source."+map[string]string{"desired_users": "username", "desired_groups": "group_name"}[table]+"))", nil, nil)...)
	}
	expression := "CAST(JSON_OBJECT('dimensions',IF(JSON_LENGTH(members)=0,JSON_ARRAY(),JSON_ARRAY(JSON_OBJECT('length',JSON_LENGTH(members),'lower_bound',1))),'elements',JSON_EXTRACT(members,'$')) AS BINARY)"
	check := "SELECT IF(NOT EXISTS(SELECT 1 FROM desired_groups WHERE logical_members IS NULL OR logical_members<>" + expression + " OR NOT ocserv_text_array_valid(logical_members)),'valid','invalid')"
	steps = append(steps,
		LongKeyStep{Name: "desired_groups_logical_column", SQL: "ALTER TABLE desired_groups ADD COLUMN logical_members LONGBLOB NULL", Kind: "table", Object: "desired_groups"},
		LongKeyStep{Name: "desired_groups_logical_backfill", SQL: "UPDATE desired_groups SET logical_members=" + expression + " WHERE logical_members IS NULL", Kind: "data", Object: "desired_groups", Repairable: true, VerifySQL: check},
	)
	drop, join := "DROP CHECK", ""
	if engine == MariaDB {
		drop, join = "DROP CONSTRAINT", " AND c.TABLE_NAME=t.TABLE_NAME AND t.CONSTRAINT_NAME<>'members'"
	}
	// Native JSON contributes engine-specific unnamed checks, which must leave
	// with the old column. The verified logical array guard replaces them.
	ddl := fmt.Sprintf(`CREATE PROCEDURE ocserv_switch_desired_groups() SQL SECURITY DEFINER
BEGIN
 DECLARE finished BOOLEAN DEFAULT FALSE;
 DECLARE constraint_name_value VARCHAR(64);
 DECLARE ddl LONGTEXT DEFAULT 'ALTER TABLE desired_groups ';
 DECLARE checks_cursor CURSOR FOR SELECT t.CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS t JOIN information_schema.CHECK_CONSTRAINTS c ON c.CONSTRAINT_SCHEMA=t.CONSTRAINT_SCHEMA AND c.CONSTRAINT_NAME=t.CONSTRAINT_NAME%s WHERE t.CONSTRAINT_SCHEMA=DATABASE() AND t.TABLE_NAME='desired_groups' AND t.CONSTRAINT_TYPE='CHECK' AND LOWER(c.CHECK_CLAUSE) LIKE '%%members%%' ORDER BY t.CONSTRAINT_NAME;
 DECLARE CONTINUE HANDLER FOR NOT FOUND SET finished=TRUE;
 OPEN checks_cursor;
 checks_loop: LOOP
  FETCH checks_cursor INTO constraint_name_value;
  IF finished THEN LEAVE checks_loop; END IF;
  SET ddl=CONCAT(ddl,'%s ',CHAR(96),REPLACE(constraint_name_value,CHAR(96),CONCAT(CHAR(96),CHAR(96))),CHAR(96),',');
 END LOOP;
 CLOSE checks_cursor;
 SET @ocserv_type_ddl=CONCAT(ddl,'DROP COLUMN members,CHANGE COLUMN logical_members members LONGBLOB NOT NULL');
 PREPARE type_statement FROM @ocserv_type_ddl;
 EXECUTE type_statement;
 DEALLOCATE PREPARE type_statement;
 SET @ocserv_type_ddl=NULL;
END`, join, drop)
	steps = append(steps,
		LongKeyStep{Name: "desired_groups_logical_switch_procedure", SQL: ddl, Kind: "procedure", Object: "ocserv_switch_desired_groups"},
		LongKeyStep{Name: "desired_groups_logical_switch", SQL: "CALL ocserv_switch_desired_groups()", Kind: "table", Object: "desired_groups", CheckBeforeSQL: check},
		LongKeyStep{Name: "desired_groups_logical_switch_cleanup", SQL: "DROP PROCEDURE ocserv_switch_desired_groups", Kind: "procedure", Object: "ocserv_switch_desired_groups"},
	)
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := "desired_groups_logical_" + strings.ToLower(event)
		ddl := fmt.Sprintf("CREATE TRIGGER %s BEFORE %s ON desired_groups FOR EACH ROW BEGIN IF NOT ocserv_text_array_valid(NEW.members) OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096 THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END", name, event)
		steps = append(steps, LongKeyStep{Name: name, SQL: ddl, Kind: "trigger", Object: name})
	}
	return GuardTimeSteps(steps, "desired_users", "desired_groups")
}
