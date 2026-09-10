package mysql

import (
	"fmt"
	"strings"
)

// Validators use native JSON only after masking decimal tokens. Thus the
// server never converts a PostgreSQL decimal to DOUBLE or bounded DECIMAL.
const jsonbObjectValidator = `CREATE FUNCTION ocserv_jsonb_object_valid(doc LONGBLOB) RETURNS BOOLEAN DETERMINISTIC NO SQL SQL SECURITY DEFINER
BEGIN
 DECLARE source LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE masked LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT '';
 DECLARE token LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE digits LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE c CHAR(1) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE posn BIGINT DEFAULT 1;
 DECLARE startn BIGINT;
 DECLARE n BIGINT;
 DECLARE dotn BIGINT;
 DECLARE en BIGINT;
 DECLARE expo BIGINT;
 DECLARE scale_n BIGINT;
 DECLARE integer_n BIGINT;
 DECLARE leading_n BIGINT;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION RETURN FALSE;
 IF doc IS NULL THEN RETURN FALSE; END IF;
 SET source=CONVERT(doc USING utf8mb4);
 IF CAST(source AS BINARY)<>doc THEN RETURN FALSE; END IF;
 SET n=CHAR_LENGTH(source);
 WHILE posn<=n DO
  SET c=SUBSTRING(source,posn,1);
  IF c='"' THEN
   SET startn=posn;
   SET posn=posn+1;
   WHILE posn<=n AND SUBSTRING(source,posn,1)<>'"' DO
    IF ASCII(SUBSTRING(source,posn,1))=92 THEN SET posn=posn+1; END IF;
    SET posn=posn+1;
   END WHILE;
   IF posn>n THEN RETURN FALSE; END IF;
   SET token=SUBSTRING(source,startn,posn-startn+1);
   IF NOT JSON_VALID(token) OR LOCATE(0x00,CAST(JSON_UNQUOTE(token) AS BINARY))<>0 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,token);
   SET posn=posn+1;
  ELSEIF c='-' OR (ASCII(c)>=48 AND ASCII(c)<=57) THEN
   SET startn=posn;
   WHILE posn<=n AND LOCATE(SUBSTRING(source,posn,1),'0123456789.eE+-')>0 DO SET posn=posn+1; END WHILE;
   SET token=SUBSTRING(source,startn,posn-startn);
   IF NOT (token REGEXP '(?-i)\\A-?(0|[1-9][0-9]*)(\\.[0-9]+)?([eE][+-]?[0-9]+)?\\z') THEN RETURN FALSE; END IF;
   IF LEFT(token,1)='-' THEN SET token=SUBSTRING(token,2); END IF;
   SET en=LOCATE('e',LOWER(token)); SET expo=0;
   IF en>0 THEN
    SET digits=SUBSTRING(token,en+1);
    IF LEFT(digits,1) IN ('-','+') THEN SET digits=SUBSTRING(digits,2); END IF;
    SET digits=TRIM(LEADING '0' FROM digits);
    IF CHAR_LENGTH(digits)>10 THEN RETURN FALSE; END IF;
    IF digits<>'' AND CAST(digits AS UNSIGNED)>1073741823 THEN RETURN FALSE; END IF;
    SET expo=CAST(SUBSTRING(token,en+1) AS SIGNED);
    SET token=LEFT(token,en-1);
   END IF;
   SET dotn=LOCATE('.',token);
   SET scale_n=IF(dotn=0,0,CHAR_LENGTH(token)-dotn)-expo;
   IF scale_n>16383 THEN RETURN FALSE; END IF;
   SET digits=REPLACE(token,'.','');
   SET leading_n=CHAR_LENGTH(digits)-CHAR_LENGTH(TRIM(LEADING '0' FROM digits));
   SET integer_n=IF(dotn=0,CHAR_LENGTH(token),dotn-1)+expo-leading_n;
   IF leading_n<>CHAR_LENGTH(digits) AND integer_n>131072 THEN RETURN FALSE; END IF;
   SET masked=CONCAT(masked,'0');
  ELSE
   SET masked=CONCAT(masked,c); SET posn=posn+1;
  END IF;
 END WHILE;
 IF NOT JSON_VALID(masked) THEN RETURN FALSE; END IF;
 RETURN CAST(JSON_TYPE(masked) AS BINARY)=_binary'OBJECT';
END`

const textArrayValidator = `CREATE FUNCTION ocserv_text_array_valid(doc LONGBLOB) RETURNS BOOLEAN DETERMINISTIC NO SQL SQL SECURITY DEFINER
BEGIN
 DECLARE source LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE dims LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE elems LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE item LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
 DECLARE dimension_n BIGINT;
 DECLARE length_n BIGINT;
 DECLARE lower_n BIGINT;
 DECLARE total_n BIGINT DEFAULT 1;
 DECLARE i BIGINT DEFAULT 0;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION RETURN FALSE;
 IF doc IS NULL THEN RETURN FALSE; END IF;
 SET source=CONVERT(doc USING utf8mb4);
 IF CAST(source AS BINARY)<>doc OR NOT JSON_VALID(source) THEN RETURN FALSE; END IF;
 IF CAST(JSON_TYPE(source) AS BINARY)<>_binary'OBJECT' OR JSON_LENGTH(source)<>2 THEN RETURN FALSE; END IF;
 SET dims=JSON_EXTRACT(source,'$.dimensions'); SET elems=JSON_EXTRACT(source,'$.elements');
 IF dims IS NULL OR elems IS NULL OR CAST(JSON_TYPE(dims) AS BINARY)<>_binary'ARRAY' OR CAST(JSON_TYPE(elems) AS BINARY)<>_binary'ARRAY' THEN RETURN FALSE; END IF;
 SET dimension_n=JSON_LENGTH(dims);
 IF dimension_n>6 THEN RETURN FALSE; END IF;
 IF dimension_n=0 THEN SET total_n=0; END IF;
 WHILE i<dimension_n DO
  SET item=JSON_EXTRACT(dims,CONCAT('$[',i,']'));
  IF CAST(JSON_TYPE(item) AS BINARY)<>_binary'OBJECT' OR JSON_LENGTH(item)<>2 OR JSON_EXTRACT(item,'$.length') IS NULL OR JSON_EXTRACT(item,'$.lower_bound') IS NULL THEN RETURN FALSE; END IF;
  IF CAST(JSON_TYPE(JSON_EXTRACT(item,'$.length')) AS BINARY)<>_binary'INTEGER' OR CAST(JSON_TYPE(JSON_EXTRACT(item,'$.lower_bound')) AS BINARY)<>_binary'INTEGER' THEN RETURN FALSE; END IF;
  SET length_n=CAST(JSON_UNQUOTE(JSON_EXTRACT(item,'$.length')) AS SIGNED);
  SET lower_n=CAST(JSON_UNQUOTE(JSON_EXTRACT(item,'$.lower_bound')) AS SIGNED);
  IF length_n<=0 OR length_n>134217727 OR lower_n < -2147483648 OR lower_n>2147483647 OR lower_n+length_n>2147483647 OR total_n>134217727 DIV length_n THEN RETURN FALSE; END IF;
  SET total_n=total_n*length_n; SET i=i+1;
 END WHILE;
 IF JSON_LENGTH(elems)<>total_n THEN RETURN FALSE; END IF;
 SET i=0;
 WHILE i<total_n DO
  SET item=JSON_EXTRACT(elems,CONCAT('$[',i,']'));
  IF CAST(JSON_TYPE(item) AS BINARY) NOT IN (_binary'STRING',_binary'NULL') THEN RETURN FALSE; END IF;
  IF CAST(JSON_TYPE(item) AS BINARY)=_binary'STRING' AND LOCATE(0x00,CAST(JSON_UNQUOTE(item) AS BINARY))<>0 THEN RETURN FALSE; END IF;
  SET i=i+1;
 END WHILE;
 RETURN TRUE;
END`

// TypeMigrationSteps is append-only authoring input. Backfill statements leave
// original columns intact until value-level verification succeeds. In
// particular, year 1000 remains a finite instant and is never guessed infinity.
func TypeMigrationSteps(engine Engine) []LongKeyStep {
	steps := []LongKeyStep{
		{Name: "jsonb_object_validator", SQL: jsonbObjectValidator, Kind: "function", Object: "ocserv_jsonb_object_valid"},
		{Name: "text_array_validator", SQL: textArrayValidator, Kind: "function", Object: "ocserv_text_array_valid"},
	}
	for _, tc := range [][2]string{{"observed_groups", "observed_at"}, {"telemetry_security_events", "observed_at"}} {
		table, col := tc[0], tc[1]
		temp := "logical_" + col
		steps = append(steps, LongKeyStep{Name: table + "_time_column", SQL: fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` BIGINT NULL", table, temp), Kind: "table", Object: table})
		steps = append(steps, LongKeyStep{Name: table + "_time_backfill", SQL: fmt.Sprintf("UPDATE `%s` SET `%s`=TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',`%s`) WHERE `%s` IS NULL", table, temp, col, temp), Kind: "data", Object: table, Repairable: true, VerifySQL: fmt.Sprintf("SELECT IF(NOT EXISTS(SELECT 1 FROM `%s` WHERE `%s` IS NULL OR `%s`<>TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',`%s`)),'valid','invalid')", table, temp, temp, col)})
		indexBefore, indexAfter := "", ""
		if table == "telemetry_security_events" {
			indexBefore = "DROP INDEX telemetry_security_node_time_idx,"
			indexAfter = ",ADD KEY telemetry_security_node_time_idx(node_id,observed_at DESC)"
		}
		steps = append(steps, LongKeyStep{Name: table + "_time_switch", SQL: fmt.Sprintf("ALTER TABLE `%s` %s DROP COLUMN `%s`,CHANGE COLUMN `%s` `%s` BIGINT NOT NULL,ADD CONSTRAINT `%s_time_range` CHECK (`%s` IN (-9223372036854775808,9223372036854775807) OR (`%s`>=-211813488000000000 AND `%s`<9223371331200000000))%s", table, indexBefore, col, temp, col, table, col, col, col, indexAfter), Kind: "table", Object: table})
		steps[len(steps)-1].CheckBeforeSQL = steps[len(steps)-2].VerifySQL
	}
	for _, bc := range [][3]string{{"observed_groups", "members", "ocserv_text_array_valid"}, {"telemetry_security_events", "detail", "ocserv_jsonb_object_valid"}} {
		table, col, validator := bc[0], bc[1], bc[2]
		temp := "logical_" + col
		expression := "CAST(`" + col + "` AS BINARY)"
		if col == "members" {
			expression = "CAST(JSON_OBJECT('dimensions',IF(JSON_LENGTH(members)=0,JSON_ARRAY(),JSON_ARRAY(JSON_OBJECT('length',JSON_LENGTH(members),'lower_bound',1))),'elements',JSON_EXTRACT(members,'$')) AS BINARY)"
		}
		steps = append(steps, LongKeyStep{Name: table + "_logical_column", SQL: fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` LONGBLOB NULL", table, temp), Kind: "table", Object: table})
		steps = append(steps, LongKeyStep{Name: table + "_logical_backfill", SQL: fmt.Sprintf("UPDATE `%s` SET `%s`=%s WHERE `%s` IS NULL", table, temp, expression, temp), Kind: "data", Object: table, Repairable: true, VerifySQL: fmt.Sprintf("SELECT IF(NOT EXISTS(SELECT 1 FROM `%s` WHERE `%s` IS NULL OR `%s`<>%s OR NOT %s(`%s`)),'valid','invalid')", table, temp, temp, expression, validator, temp)})
		copyCheck := steps[len(steps)-1].VerifySQL
		procedure := "ocserv_switch_" + table
		drop := "DROP CHECK"
		if engine == MariaDB {
			drop = "DROP CONSTRAINT"
		}
		joinTable := ""
		if engine == MariaDB {
			joinTable = " AND c.TABLE_NAME=t.TABLE_NAME AND t.CONSTRAINT_NAME<>'" + col + "'"
		}
		ddl := fmt.Sprintf(`CREATE PROCEDURE %s() SQL SECURITY DEFINER
BEGIN
 DECLARE finished BOOLEAN DEFAULT FALSE;
 DECLARE constraint_name_value VARCHAR(64);
 DECLARE ddl LONGTEXT DEFAULT 'ALTER TABLE %s ';
 DECLARE checks_cursor CURSOR FOR SELECT t.CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS t JOIN information_schema.CHECK_CONSTRAINTS c ON c.CONSTRAINT_SCHEMA=t.CONSTRAINT_SCHEMA AND c.CONSTRAINT_NAME=t.CONSTRAINT_NAME%s WHERE t.CONSTRAINT_SCHEMA=DATABASE() AND t.TABLE_NAME='%s' AND t.CONSTRAINT_TYPE='CHECK' AND LOWER(c.CHECK_CLAUSE) LIKE '%%%s%%' ORDER BY t.CONSTRAINT_NAME;
 DECLARE CONTINUE HANDLER FOR NOT FOUND SET finished=TRUE;
 OPEN checks_cursor;
 checks_loop: LOOP
  FETCH checks_cursor INTO constraint_name_value;
  IF finished THEN LEAVE checks_loop; END IF;
  SET ddl=CONCAT(ddl,'%s ',CHAR(96),REPLACE(constraint_name_value,CHAR(96),CONCAT(CHAR(96),CHAR(96))),CHAR(96),',');
 END LOOP;
 CLOSE checks_cursor;
 SET @ocserv_type_ddl=CONCAT(ddl,'DROP COLUMN %s,CHANGE COLUMN %s %s LONGBLOB NOT NULL');
 PREPARE type_statement FROM @ocserv_type_ddl;
 EXECUTE type_statement;
 DEALLOCATE PREPARE type_statement;
 SET @ocserv_type_ddl=NULL;
END`, procedure, table, joinTable, table, col, drop, col, temp, col)
		steps = append(steps, LongKeyStep{Name: table + "_logical_switch_procedure", SQL: ddl, Kind: "procedure", Object: procedure})
		steps = append(steps, LongKeyStep{Name: table + "_logical_switch", SQL: "CALL " + procedure + "()", Kind: "table", Object: table, CheckBeforeSQL: copyCheck})
		steps = append(steps, LongKeyStep{Name: table + "_logical_switch_cleanup", SQL: "DROP PROCEDURE " + procedure, Kind: "procedure", Object: procedure})
		for _, event := range []string{"INSERT", "UPDATE"} {
			name := table + "_logical_" + strings.ToLower(event)
			extra := ""
			if col == "members" {
				extra = " OR JSON_LENGTH(JSON_EXTRACT(CONVERT(NEW.members USING utf8mb4),'$.elements'))>4096"
			}
			sql := fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON `%s` FOR EACH ROW BEGIN IF NOT %s(NEW.`%s`)%s THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid logical value'; END IF; END", name, event, table, validator, col, extra)
			steps = append(steps, LongKeyStep{Name: name, SQL: sql, Kind: "trigger", Object: name})
		}
	}
	return steps
}
