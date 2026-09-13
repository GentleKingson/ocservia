package mysql

// This nonunique prefix is only an access path. The long-key fallback compares
// the entire encoded key under its guard, including beyond the prefix.
func TelemetryLookupSteps(engine Engine) []LongKeyStep {
	var steps []LongKeyStep
	for _, suffix := range []string{"5m", "1h"} {
		table := "exact_telemetry_rollups_" + suffix
		steps = append(steps, LongKeyStep{Name: "telemetry_" + suffix + "_key_lookup", Object: table, Kind: "table", SQL: "ALTER TABLE " + table + " ADD INDEX telemetry_key_lookup(key_value(255))"})
	}
	// A start_at scan can lock current months while searching for an expired
	// candidate, cycling with ingestion's catalog lock and node FK locks.
	steps = append(steps, LongKeyStep{Name: "telemetry_retirement_lookup", Object: "telemetry_sample_shards", Kind: "table", SQL: "ALTER TABLE telemetry_sample_shards ADD INDEX telemetry_retirement_lookup(state,end_at,start_at)"})
	steps = append(steps, LongKeyStep{Name: "telemetry_retire_before_v26", Object: "telemetry_retire_shards", Kind: "procedure", SQL: "DROP PROCEDURE telemetry_retire_shards"})
	steps = append(steps, telemetryBatchSteps(engine)...)
	steps = append(steps, telemetryShortKeySteps()...)
	return steps
}
