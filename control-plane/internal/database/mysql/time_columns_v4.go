package mysql

import (
	"fmt"
	"strings"
)

// TimeColumn is an authoring declaration, never an inferred SQL rewrite.
// DefaultSQL is empty unless the domain has an explicit logical default.
type TimeColumn struct {
	Name           string
	Nullable       bool
	DefaultSQL     string
	LegacyInfinity bool
}

func TimeDecisionSteps() []LongKeyStep {
	return []LongKeyStep{{Name: "time_migration_decisions", Kind: "table", Object: "time_migration_decisions", SQL: `CREATE TABLE time_migration_decisions (
 table_name VARBINARY(64) NOT NULL,
 column_name VARBINARY(64) NOT NULL,
 row_key VARBINARY(128) NOT NULL,
 source_value DATETIME(6) NOT NULL,
 decision VARBINARY(24) NOT NULL,
 PRIMARY KEY(table_name,column_name,row_key),
 CHECK(decision IN ('finite','negative_infinity'))
) ENGINE=InnoDB`}}
}

// TimeColumnSteps preserves source columns until a guarded, verified switch.
// rowKeySQL and index clauses are reviewed backend-owned SQL, not user input.
// The present decision keys are scheduler's singleton id or the already
// PostgreSQL-constrained 128-byte login name; this is not a general key codec.
func TimeColumnSteps(table string, columns []TimeColumn, rowKeySQL string, dropClauses, addClauses []string) []LongKeyStep {
	return GuardTimeSteps(timeColumnSteps(table, columns, rowKeySQL, dropClauses, addClauses), table)
}

func timeColumnSteps(table string, columns []TimeColumn, rowKeySQL string, dropClauses, addClauses []string) []LongKeyStep {
	var steps []LongKeyStep
	var copies, switches, checks []string
	for _, c := range columns {
		if !identifier.MatchString(table) || !identifier.MatchString(c.Name) || len("logical_"+c.Name) > 64 {
			panic("invalid time migration declaration")
		}
		shadow := "logical_" + c.Name
		steps = append(steps, LongKeyStep{Name: table + "_" + c.Name + "_column", Kind: "table", Object: table, SQL: fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` BIGINT NULL", table, shadow)})
		expression := fmt.Sprintf("TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',source.`%s`)", c.Name)
		if c.LegacyInfinity {
			decision := fmt.Sprintf("(SELECT d.decision FROM time_migration_decisions d WHERE d.table_name='%s' AND d.column_name='%s' AND d.row_key=CAST(%s AS BINARY) AND d.source_value=source.`%s`)", table, c.Name, rowKeySQL, c.Name)
			expression = fmt.Sprintf("CASE WHEN source.`%s`='1000-01-01 00:00:00.000000' THEN CASE %s WHEN 'finite' THEN %s WHEN 'negative_infinity' THEN -9223372036854775808 ELSE NULL END ELSE %s END", c.Name, decision, expression, expression)
		}
		copies = append(copies, fmt.Sprintf("`%s`=%s", shadow, expression))
		checks = append(checks, fmt.Sprintf("(source.`%s` IS NOT NULL AND source.`%s` IS NULL) OR NOT(source.`%s` <=> %s)", c.Name, shadow, shadow, expression))
		nullability := "NOT NULL"
		if c.Nullable {
			nullability = "NULL"
		}
		defaultSQL := ""
		if c.DefaultSQL != "" {
			defaultSQL = " DEFAULT " + c.DefaultSQL
		}
		switches = append(switches, fmt.Sprintf("DROP COLUMN `%s`,CHANGE COLUMN `%s` `%s` BIGINT %s%s,ADD CONSTRAINT `%s_%s_range` CHECK (`%s` IS NULL OR `%s` IN (-9223372036854775808,9223372036854775807) OR (`%s`>=-211813488000000000 AND `%s`<9223371331200000000))", c.Name, shadow, c.Name, nullability, defaultSQL, table, c.Name, c.Name, c.Name, c.Name, c.Name))
	}
	verify := fmt.Sprintf("SELECT IF(NOT EXISTS(SELECT 1 FROM `%s` source WHERE %s),'valid','invalid')", table, strings.Join(checks, " OR "))
	steps = append(steps, LongKeyStep{Name: table + "_times_backfill", Kind: "data", Object: table, Repairable: true, SQL: fmt.Sprintf("UPDATE `%s` source SET %s", table, strings.Join(copies, ",")), VerifySQL: verify})
	clauses := append(append(append([]string{}, dropClauses...), switches...), addClauses...)
	steps = append(steps, LongKeyStep{Name: table + "_times_switch", Kind: "table", Object: table, CheckBeforeSQL: verify, SQL: fmt.Sprintf("ALTER TABLE `%s` %s", table, strings.Join(clauses, ","))})
	return steps
}

func GuardTimeSteps(changes []LongKeyStep, tables ...string) []LongKeyStep {
	var steps []LongKeyStep
	for _, table := range tables {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "migrate_v4_time_" + table + "_" + strings.ToLower(event)
			body := fmt.Sprintf("CREATE TRIGGER `%s` BEFORE %s ON `%s` FOR EACH ROW BEGIN DECLARE holder BIGINT; SET holder=IS_USED_LOCK(CONCAT('ocservia:',LEFT(SHA2(DATABASE(),256),48))); IF holder IS NULL OR holder<>CONNECTION_ID() THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='time migration requires exclusive writer'; END IF; END", name, event, table)
			steps = append(steps, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: body})
		}
	}
	steps = append(steps, changes...)
	for _, table := range tables {
		for _, event := range []string{"insert", "update", "delete"} {
			name := "migrate_v4_time_" + table + "_" + event
			steps = append(steps, LongKeyStep{Name: "drop_" + name, Kind: "trigger", Object: name, SQL: "DROP TRIGGER `" + name + "`"})
		}
	}
	return steps
}
