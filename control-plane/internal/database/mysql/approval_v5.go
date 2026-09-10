package mysql

// ApprovalRemainingSteps appends lossless storage for the remaining approval
// fields. Published revisions remain byte-for-byte unchanged.
func ApprovalRemainingSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	steps := timeColumnSteps("approval_requests", []TimeColumn{{Name: "approved_at", Nullable: true}, {Name: "authority_snapshot_at", DefaultSQL: "(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))"}}, "HEX(source.id)", nil, nil)
	check := `SELECT IF(NOT EXISTS(SELECT 1 FROM approval_requests WHERE NOT(logical_request_summary <=> CAST(request_summary AS BINARY)) OR (logical_request_summary IS NOT NULL AND NOT ocserv_jsonb_value_valid(logical_request_summary))),'valid','invalid')`
	changes := []LongKeyStep{
		{Name: "approval_summary_column", Kind: "table", Object: "approval_requests", SQL: `ALTER TABLE approval_requests ADD COLUMN logical_request_summary LONGBLOB NULL`},
		{Name: "approval_summary_backfill", Kind: "data", Object: "approval_requests", Repairable: true, SQL: `UPDATE approval_requests SET logical_request_summary=CAST(request_summary AS BINARY)`, VerifySQL: check},
		{Name: "approval_summary_switch", Kind: "table", Object: "approval_requests", CheckBeforeSQL: check, SQL: `ALTER TABLE approval_requests ` + drop + `approval_requests_approval_request_content_pair,` + drop + `approval_requests_approval_requests_request_summary_check,DROP COLUMN request_summary,CHANGE COLUMN logical_request_summary request_summary LONGBLOB NULL,ADD CONSTRAINT approval_requests_approval_request_content_pair CHECK ((request_hash IS NULL)=(request_summary IS NULL))`},
	}
	for _, event := range []struct{ suffix, sql string }{{"insert", "INSERT"}, {"update", "UPDATE"}} {
		name := "approval_summary_" + event.suffix
		changes = append(changes, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: `CREATE TRIGGER ` + name + ` BEFORE ` + event.sql + ` ON approval_requests FOR EACH ROW BEGIN IF NEW.request_summary IS NOT NULL AND (NOT ocserv_jsonb_value_valid(NEW.request_summary) OR LEFT(TRIM(REPLACE(REPLACE(REPLACE(CONVERT(NEW.request_summary USING utf8mb4),CHAR(9),' '),CHAR(10),' '),CHAR(13),' ')),1) NOT IN ('[','{')) THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=3819,MESSAGE_TEXT='invalid approval JSON'; END IF; END`})
	}
	return GuardTimeSteps(append(steps, changes...), "approval_requests")
}
