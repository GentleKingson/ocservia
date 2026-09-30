package mysql

import "strings"

// Existing append-only fields remain immutable. Runtime has no direct UPDATE
// rights on any compaction field; only these fixed definer routines can clear
// aged detail. The extra MAC is verified by the application, never by SQL.
func AuditRetentionSteps() []LongKeyStep {
	fields := []string{"id", "workspace_id", "occurred_at", "actor_type", "actor_id", "source_session_id", "action", "resource_type", "resource_id", "node_id", "request_id", "trace_id", "command_id", "approval_id", "result", "error_type", "previous_event_hash", "event_hash", "auth_version", "event_key_id", "event_mac"}
	equal := []string{}
	for _, field := range fields {
		equal = append(equal, "(CAST(NEW.`"+field+"` AS BINARY)<=>CAST(OLD.`"+field+"` AS BINARY))")
	}
	now := "CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED)"
	floor := "7776000000000" // 90 days in microseconds
	trigger := `CREATE TRIGGER audit_events_reject_update BEFORE UPDATE ON audit_events FOR EACH ROW BEGIN IF NOT (
 OLD.details_compacted_at IS NULL AND OLD.auth_version=1 AND BINARY OLD.action<>BINARY 'audit.auth.transition'
 AND NEW.details_compacted_at IS NOT NULL AND OLD.occurred_at<=NEW.details_compacted_at-` + floor + `
 AND NEW.details_compacted_at<=` + now + `
 AND NEW.reason IS NULL AND NEW.before_summary IS NULL AND NEW.after_summary IS NULL
 AND ` + strings.Join(equal, " AND ") + `) THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='audit permits only authenticated aged detail compaction'; END IF; END`
	changes := []LongKeyStep{
		{Name: "audit_compaction_columns", Object: "audit_events", Kind: "table", SQL: `ALTER TABLE audit_events ADD COLUMN details_compacted_at BIGINT NULL, ADD COLUMN compaction_key_id VARBINARY(128) NULL, ADD COLUMN compaction_mac VARBINARY(32) NULL, ADD CONSTRAINT audit_compaction_evidence CHECK ((details_compacted_at IS NULL AND compaction_key_id IS NULL AND compaction_mac IS NULL) OR (details_compacted_at IS NOT NULL AND compaction_key_id IS NOT NULL AND compaction_mac IS NOT NULL AND OCTET_LENGTH(compaction_key_id) BETWEEN 1 AND 128 AND OCTET_LENGTH(compaction_mac)=32 AND auth_version=1 AND reason IS NULL AND before_summary IS NULL AND after_summary IS NULL)), ADD INDEX audit_detail_retention_idx(details_compacted_at,occurred_at,id)`},
		{Name: "audit_compaction_drop_guard", Object: "audit_events_reject_update", Kind: "trigger", SQL: "DROP TRIGGER audit_events_reject_update"},
		{Name: "audit_compaction_guard", Object: "audit_events_reject_update", Kind: "trigger", SQL: trigger},
		{Name: "audit_compact_detail", Object: "audit_compact_detail", Kind: "procedure", SQL: `CREATE PROCEDURE audit_compact_detail(IN p_id VARBINARY(16),IN p_hash VARBINARY(32),IN p_key VARBINARY(128),IN p_mac VARBINARY(32),IN p_at BIGINT) SQL SECURITY DEFINER BEGIN UPDATE audit_events SET reason=NULL,before_summary=NULL,after_summary=NULL,details_compacted_at=p_at,compaction_key_id=p_key,compaction_mac=p_mac WHERE id=p_id AND event_hash=p_hash AND details_compacted_at IS NULL AND auth_version=1 AND BINARY action<>BINARY 'audit.auth.transition' AND occurred_at<=p_at-` + floor + ` AND p_at<=` + now + `; SELECT ROW_COUNT()=1; END`},
		{Name: "security_detail_columns", Object: "telemetry_security_events", Kind: "table", SQL: `ALTER TABLE telemetry_security_events ADD COLUMN details_compacted_at BIGINT NULL, ADD COLUMN detail_sha256 VARBINARY(32) NULL, ADD CONSTRAINT security_detail_evidence CHECK ((details_compacted_at IS NULL AND detail_sha256 IS NULL) OR (details_compacted_at IS NOT NULL AND detail_sha256 IS NOT NULL AND OCTET_LENGTH(detail_sha256)=32 AND BINARY detail=BINARY '{}')), ADD INDEX security_detail_retention_idx(details_compacted_at,observed_at,event_id)`},
		{Name: "security_compact_details", Object: "security_compact_details", Kind: "procedure", SQL: `CREATE PROCEDURE security_compact_details(IN p_cutoff BIGINT) SQL SECURITY DEFINER BEGIN UPDATE telemetry_security_events SET detail_sha256=UNHEX(SHA2(detail,256)),detail='{}',details_compacted_at=` + now + ` WHERE details_compacted_at IS NULL AND observed_at<LEAST(p_cutoff,` + now + `-` + floor + `) ORDER BY observed_at,event_id LIMIT 32; END`},
	}
	return GuardTimeSteps(changes, "audit_events", "telemetry_security_events")
}
