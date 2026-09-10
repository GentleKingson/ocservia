package mysql

import (
	"context"
	"fmt"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

// longKeyDefinition describes one PostgreSQL natural unique key which cannot
// be represented by a MySQL bounded index. The original values remain in the
// original columns. A private, cascading side table owns the exact key bytes.
type longKeyDefinition struct {
	table, owner, ownerType, index, predicate string
	columns, widen                            []string
	surrogate                                 bool
}

var longKeys = []longKeyDefinition{
	{"identities", "id", "VARBINARY(16)", "identities_issuer_subject_key", "TRUE", []string{"issuer", "subject"}, []string{"issuer", "subject"}, false},
	{"workspaces", "id", "VARBINARY(16)", "workspaces_slug_key", "TRUE", []string{"slug"}, []string{"slug"}, false},
	{"nodes", "id", "VARBINARY(16)", "nodes_workspace_id_name_key", "TRUE", []string{"workspace_id", "name"}, []string{"name"}, false},
	{"operations", "id", "VARBINARY(16)", "operations_workspace_idempotency_idx", "idempotency_key IS NOT NULL", []string{"workspace_id", "idempotency_key"}, []string{"idempotency_key"}, false},
	{"agent_command_results", "event_id", "VARBINARY(16)", "agent_command_results_effect_receipt_unique_idx", "receipt_verification_status = 'verified' AND privd_attestation_key_id IS NOT NULL AND effect_record_id IS NOT NULL AND effect_sequence IS NOT NULL", []string{"privd_attestation_key_id", "effect_record_id", "effect_sequence"}, []string{"privd_attestation_key_id"}, false},
	{"upstream_sync_records", "id", "VARBINARY(16)", "upstream_sync_records_repository_old_commit_new_commit_key", "TRUE", []string{"repository", "old_commit", "new_commit"}, []string{"repository"}, false},
	{"user_policy_enforcements", "exact_row_id", "BIGINT UNSIGNED", "PRIMARY", "TRUE", []string{"node_id", "username", "policy_version", "cause", "period_start"}, []string{"username"}, true},
	{"telemetry_rollups_5m", "exact_row_id", "BIGINT UNSIGNED", "PRIMARY", "TRUE", []string{"node_id", "metric", "bucket_at"}, []string{"metric"}, true},
	{"telemetry_rollups_1h", "exact_row_id", "BIGINT UNSIGNED", "PRIMARY", "TRUE", []string{"node_id", "metric", "bucket_at"}, []string{"metric"}, true},
}

// LongKeyStep is an authoring input for an append-only manifest, not a migration
// executor. Object fingerprints must be captured and reviewed independently on
// each pinned engine before these statements can enter a published revision.
type LongKeyStep struct {
	Name, SQL, Kind, Object string
	VerifySQL               string
	CheckBeforeSQL          string
	Repairable              bool
}

func longKeyBytes(columns []string, prefix string) string {
	parts := make([]string, 0, len(columns)*2)
	for _, column := range columns {
		value := "CAST(" + prefix + "`" + column + "` AS BINARY)"
		parts = append(parts, "UNHEX(LPAD(HEX(OCTET_LENGTH("+value+")),16,'0'))", value)
	}
	return "CONCAT(" + strings.Join(parts, ",") + ")"
}

func longKeyPredicate(d longKeyDefinition, prefix string) string {
	predicate := d.predicate
	for _, column := range []string{"idempotency_key", "receipt_verification_status", "privd_attestation_key_id", "effect_record_id", "effect_sequence"} {
		predicate = strings.ReplaceAll(predicate, column, prefix+"`"+column+"`")
	}
	return predicate
}

// LongKeyMigrationSteps retains full binary equality, including case and tail
// spaces. A transaction-held guard serializes each constraint, not each hash.
// The side table has no hash or prefix unique index and deletes with its owner,
// including InnoDB FK cascades which do not execute child-table triggers.
func LongKeyMigrationSteps() []LongKeyStep {
	steps := []LongKeyStep{{Name: "exact_key_guards", SQL: "CREATE TABLE exact_key_guards (key_name VARBINARY(64) NOT NULL PRIMARY KEY) ENGINE=InnoDB", Kind: "table", Object: "exact_key_guards"}}
	// MySQL requires a write privilege for SELECT ... FOR UPDATE. Grant only
	// UPDATE(key_name), while this trigger rejects every actual update, even
	// no-ops. Runtime still has neither INSERT nor DELETE on guard rows.
	steps = append(steps, LongKeyStep{Name: "exact_key_guards_immutable", SQL: "CREATE TRIGGER exact_key_guards_immutable BEFORE UPDATE ON exact_key_guards FOR EACH ROW SIGNAL SQLSTATE '42000' SET MYSQL_ERRNO=1142,MESSAGE_TEXT='exact-key guards are immutable'", Kind: "trigger", Object: "exact_key_guards_immutable"})
	for _, d := range longKeys {
		add := func(suffix, sql, kind, object string) {
			steps = append(steps, LongKeyStep{Name: d.table + "_" + suffix, SQL: sql, Kind: kind, Object: object})
		}
		if d.surrogate {
			lookup := "node_id"
			if strings.HasPrefix(d.table, "telemetry_rollups_") {
				lookup += ",bucket_at"
			}
			add("owner", "ALTER TABLE `"+d.table+"` ADD COLUMN exact_row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, ADD UNIQUE KEY exact_row_id_key (exact_row_id), ADD KEY exact_node_lookup ("+lookup+")", "table", d.table)
		}
		side := "exact_" + d.table
		add("guard", "INSERT INTO exact_key_guards(key_name) VALUES ('"+d.table+"')", "data", "exact_key_guards")
		steps[len(steps)-1].VerifySQL = "SELECT IF(COUNT(*)=1,'valid','invalid') FROM exact_key_guards WHERE key_name='" + d.table + "'"
		steps[len(steps)-1].Repairable = true
		add("keys", "CREATE TABLE `"+side+"` (owner_id "+d.ownerType+" NOT NULL PRIMARY KEY, key_value LONGBLOB NOT NULL, CONSTRAINT `"+side+"_owner_fk` FOREIGN KEY (owner_id) REFERENCES `"+d.table+"` (`"+d.owner+"`) ON DELETE CASCADE ON UPDATE CASCADE) ENGINE=InnoDB", "table", side)
		add("backfill", "INSERT INTO `"+side+"` (owner_id,key_value) SELECT source.`"+d.owner+"`,"+longKeyBytes(d.columns, "source.")+" FROM `"+d.table+"` source WHERE "+longKeyPredicate(d, "source.")+" AND NOT EXISTS (SELECT 1 FROM `"+side+"` existing WHERE existing.owner_id=source.`"+d.owner+"`)", "data", side)
		steps[len(steps)-1].VerifySQL = "SELECT IF(NOT EXISTS(SELECT 1 FROM `" + d.table + "` source LEFT JOIN `" + side + "` existing ON existing.owner_id=source.`" + d.owner + "` WHERE " + longKeyPredicate(d, "source.") + " AND (existing.owner_id IS NULL OR existing.key_value<>" + longKeyBytes(d.columns, "source.") + ")) AND NOT EXISTS(SELECT 1 FROM `" + side + "` existing JOIN `" + d.table + "` source ON source.`" + d.owner + "`=existing.owner_id WHERE NOT (" + longKeyPredicate(d, "source.") + ")),'valid','invalid')"
		steps[len(steps)-1].Repairable = true
		copyCheck := steps[len(steps)-1].VerifySQL
		for _, event := range []string{"INSERT", "UPDATE"} {
			name := "exact_" + d.table + "_" + strings.ToLower(event)
			body := "CREATE TRIGGER `" + name + "` AFTER " + event + " ON `" + d.table + "` FOR EACH ROW BEGIN " +
				"DECLARE guard_name VARBINARY(64); DECLARE duplicate_owner " + d.ownerType + "; DECLARE encoded LONGBLOB; " +
				"DECLARE CONTINUE HANDLER FOR NOT FOUND SET duplicate_owner=NULL; " +
				"SELECT key_name INTO guard_name FROM exact_key_guards WHERE key_name='" + d.table + "' FOR UPDATE; " +
				"IF guard_name IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF; "
			if event == "UPDATE" {
				body += "DELETE FROM `" + side + "` WHERE owner_id=NEW.`" + d.owner + "`; "
			}
			body += "IF " + longKeyPredicate(d, "NEW.") + " THEN SET encoded=" + longKeyBytes(d.columns, "NEW.") + "; " +
				"SELECT owner_id INTO duplicate_owner FROM `" + side + "` WHERE key_value=encoded LIMIT 1 FOR UPDATE; " +
				"IF duplicate_owner IS NOT NULL THEN SIGNAL SQLSTATE '23000' SET MYSQL_ERRNO=1062,MESSAGE_TEXT='duplicate exact natural key'; END IF; " +
				"INSERT INTO `" + side + "` (owner_id,key_value) VALUES(NEW.`" + d.owner + "`,encoded); END IF; END"
			add(strings.ToLower(event), body, "trigger", name)
		}
		alter := "ALTER TABLE `" + d.table + "` "
		if d.surrogate {
			alter += "DROP PRIMARY KEY, ADD PRIMARY KEY (exact_row_id)"
		} else {
			alter += "DROP INDEX `" + d.index + "`"
		}
		for _, column := range d.widen {
			null := " NOT NULL"
			if column == "idempotency_key" || column == "privd_attestation_key_id" {
				null = " NULL"
			}
			alter += ", MODIFY COLUMN `" + column + "` LONGTEXT" + null
		}
		add("unbounded", alter, "table", d.table)
		steps[len(steps)-1].CheckBeforeSQL = copyCheck
	}
	steps = append(steps, LongKeyStep{Name: "commands_resource_key", SQL: "ALTER TABLE commands DROP INDEX commands_pending_resource_idx, MODIFY COLUMN resource_key LONGTEXT NULL, ADD KEY commands_pending_resource_idx (node_id,resource_type,created_at)", Kind: "table", Object: "commands"})
	return steps
}

// LockExactKey precedes a natural-key lookup followed by insert/update in a
// borrowed business transaction. ON DUPLICATE KEY cannot implement upsert for
// these keys: their exact uniqueness is enforced by a trigger, not an index.
func LockExactKey(ctx context.Context, tx database.Tx, table string) error {
	for _, d := range longKeys {
		if d.table == table {
			var key []byte
			return tx.QueryRow(ctx, "SELECT key_name FROM exact_key_guards WHERE key_name=? FOR UPDATE", []byte(table)).Scan(&key)
		}
	}
	return fmt.Errorf("unknown exact natural key: %w", database.ErrUnsupported)
}
