package mysql

import "strings"

// Short metrics use native uniqueness over the complete natural key, never a
// prefix or hash. Long metrics map to NULL and retain guarded full comparison.
func telemetryShortKeySteps() []LongKeyStep {
	var steps []LongKeyStep
	for _, suffix := range []string{"5m", "1h"} {
		table := "telemetry_rollups_" + suffix
		side := "exact_" + table
		steps = append(steps, LongKeyStep{Name: table + "_short_key", Object: table, Kind: "table", SQL: "ALTER TABLE " + table + " ADD COLUMN short_metric VARBINARY(255) GENERATED ALWAYS AS (CASE WHEN OCTET_LENGTH(metric)<=255 THEN CAST(metric AS BINARY) ELSE NULL END) STORED, ADD UNIQUE KEY telemetry_short_key(node_id,bucket_at,short_metric)"})
		for _, event := range []string{"INSERT", "UPDATE"} {
			name := side + "_" + strings.ToLower(event)
			steps = append(steps, LongKeyStep{Name: name + "_before_v26", Object: name, Kind: "trigger", SQL: "DROP TRIGGER " + name})
			body := "CREATE TRIGGER " + name + " AFTER " + event + " ON " + table + " FOR EACH ROW main: BEGIN DECLARE encoded LONGBLOB; DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner BIGINT UNSIGNED; DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; "
			if event == "UPDATE" {
				body += "IF NEW.node_id=OLD.node_id AND BINARY NEW.metric=BINARY OLD.metric AND NEW.bucket_at=OLD.bucket_at THEN LEAVE main; END IF; "
			} else {
				body += "IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; "
			}
			body += "SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='" + table + "' FOR UPDATE; IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; "
			if event == "UPDATE" {
				body += "DELETE FROM " + side + " WHERE owner_id=NEW.exact_row_id; "
				body += "IF OCTET_LENGTH(NEW.metric)<=255 THEN LEAVE main; END IF; "
			}
			body += "SET encoded=" + longKeyBytes([]string{"node_id", "metric", "bucket_at"}, "NEW.") + "; SELECT owner_id INTO duplicate_owner FROM " + side + " WHERE key_value=encoded LIMIT 1 FOR UPDATE; IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; INSERT INTO " + side + "(owner_id,key_value) VALUES(NEW.exact_row_id,encoded); END"
			steps = append(steps, LongKeyStep{Name: name + "_v26", Object: name, Kind: "trigger", SQL: body})
		}
		steps = append(steps, LongKeyStep{Name: table + "_short_key_receipts", Object: side, Kind: "data", Repairable: true,
			SQL:       "DELETE s FROM " + side + " s JOIN " + table + " r ON r.exact_row_id=s.owner_id WHERE OCTET_LENGTH(r.metric)<=255",
			VerifySQL: "SELECT IF(NOT EXISTS(SELECT 1 FROM " + side + " s JOIN " + table + " r ON r.exact_row_id=s.owner_id WHERE OCTET_LENGTH(r.metric)<=255),'valid','invalid')"})
	}
	return steps
}
