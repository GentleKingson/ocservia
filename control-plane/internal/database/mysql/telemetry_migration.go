package mysql

import "fmt"

// TelemetryMigrationSteps is authoring input only. Published revisions pin
// each resulting object and data postcondition separately for both engines.
func TelemetryMigrationSteps(engine Engine) ([]LongKeyStep, error) {
	collation := "utf8mb4_0900_bin"
	if engine == MariaDB {
		collation = "utf8mb4_nopad_bin"
	} else if engine != MySQL {
		return nil, ErrSchema
	}
	steps := []LongKeyStep{{Name: "telemetry_catalog_guard", Kind: "data", Object: "business_locks", SQL: "INSERT INTO business_locks(lock_key) SELECT 'telemetry-shard-catalog' WHERE NOT EXISTS(SELECT 1 FROM business_locks WHERE lock_key='telemetry-shard-catalog')", VerifySQL: "SELECT IF(COUNT(*)=1,'valid','invalid') FROM business_locks WHERE lock_key='telemetry-shard-catalog'", Repairable: true}}
	for _, field := range []struct{ table, column, primary string }{
		{"telemetry_samples", "sampled_at", "sampled_at,node_id,batch_id,metric"},
		{"telemetry_rollups_5m", "bucket_at", "node_id,metric,bucket_at"},
		{"telemetry_rollups_1h", "bucket_at", "node_id,metric,bucket_at"},
	} {
		shadow := "pr02_" + field.column
		steps = append(steps, LongKeyStep{Name: field.table + "_time_shadow", Kind: "table", Object: field.table, SQL: "ALTER TABLE " + field.table + " ADD COLUMN " + shadow + " BIGINT NULL"})
		conversion := "TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00'," + field.column + ")"
		steps = append(steps, LongKeyStep{Name: field.table + "_time_backfill", Kind: "data", Object: field.table, SQL: "UPDATE " + field.table + " SET " + shadow + "=" + conversion + " WHERE " + shadow + " IS NULL", VerifySQL: "SELECT IF(NOT EXISTS(SELECT 1 FROM " + field.table + " WHERE " + shadow + " IS NULL OR " + shadow + "<>" + conversion + "),'valid','invalid')", Repairable: true})
		extra := ""
		if field.table == "telemetry_samples" {
			extra = ", DROP INDEX telemetry_samples_query_idx, ADD INDEX telemetry_samples_query_idx(node_id,metric,sampled_at)"
		}
		sql := fmt.Sprintf("ALTER TABLE %s DROP PRIMARY KEY, DROP COLUMN %s, CHANGE COLUMN %s %s BIGINT NOT NULL, ADD PRIMARY KEY(%s)%s, ADD CONSTRAINT %s_time_range CHECK(%s IN (-9223372036854775808,9223372036854775807) OR (%s>=-211813488000000000 AND %s<9223371331200000000))", field.table, field.column, shadow, field.column, field.primary, extra, field.table, field.column, field.column, field.column)
		steps = append(steps, LongKeyStep{Name: field.table + "_time_activate", Kind: "table", Object: field.table, SQL: sql, CheckBeforeSQL: steps[len(steps)-1].VerifySQL})
	}
	template, err := TelemetryShardTemplateDDL(collation)
	if err != nil {
		return nil, err
	}
	steps = append(steps,
		LongKeyStep{Name: "telemetry_sample_shards", Kind: "table", Object: "telemetry_sample_shards", SQL: TelemetryShardCatalogDDL},
		LongKeyStep{Name: "telemetry_samples_template", Kind: "table", Object: "telemetry_samples_template", SQL: template},
		LongKeyStep{Name: "telemetry_retire_shards", Kind: "procedure", Object: "telemetry_retire_shards", SQL: TelemetryRetireShardsDDL},
	)
	return steps, nil
}
