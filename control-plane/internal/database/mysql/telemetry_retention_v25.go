package mysql

import "strings"

// Retirement owns finalization, so EXECUTE alone cannot discard unaggregated
// history. Both resolutions use server-side merges over their retained window.
// Guard locks precede the candidate lock, matching recent-rollup maintenance.
func TelemetryFinalizationSteps() []LongKeyStep {
	body := `CREATE PROCEDURE telemetry_retire_shards(IN cutoff BIGINT)
SQL SECURITY DEFINER
BEGIN
 DECLARE clock_at BIGINT;
 DECLARE since_at BIGINT;
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 DECLARE candidate VARBINARY(64);
 DECLARE statement_open BOOLEAN DEFAULT FALSE;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION
 BEGIN
  IF statement_open THEN DEALLOCATE PREPARE telemetry_finalization; END IF;
  SET @telemetry_finalization_sql=NULL;
  RESIGNAL;
 END;
 SET clock_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6));
 IF cutoff IS NULL OR cutoff>clock_at-1209600000000+300000000 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry retention cutoff outside permitted window';
 END IF;
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF guard_key IS NULL OR migration_state IS NULL OR migration_state<>'complete' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry history migration is incomplete';
 END IF;
 SET guard_key=NULL;
 SELECT key_name INTO guard_key FROM exact_key_guards WHERE key_name='telemetry_rollups_5m' FOR UPDATE;
 IF guard_key IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF;
 SET guard_key=NULL;
 SELECT key_name INTO guard_key FROM exact_key_guards WHERE key_name='telemetry_rollups_1h' FOR UPDATE;
 IF guard_key IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF;
 SELECT table_name INTO candidate FROM telemetry_sample_shards WHERE state='active' AND end_at<=LEAST(cutoff,clock_at-1209600000000) ORDER BY start_at LIMIT 1 FOR UPDATE;
 IF candidate IS NOT NULL THEN
  IF CONVERT(candidate USING ascii) NOT REGEXP '^telemetry_samples_(m_[0-9]{6}|x_[pn][0-9]{7})$' THEN
   SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='invalid telemetry shard name';
  END IF;
`
	for _, resolution := range []struct {
		suffix, cutoff string
		width          int64
	}{{"5m", "FLOOR((clock_at-7776000000000)/300000000)*300000000", 300000000}, {"1h", "FLOOR(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 13 MONTH))/3600000000)*3600000000", 3600000000}} {
		table := "telemetry_rollups_" + resolution.suffix
		body += " SET since_at=" + resolution.cutoff + ";\n"
		aggregate := telemetryAggregateSQL("SELECT node_id,metric,sampled_at,value FROM `SOURCE_SHARD` WHERE sampled_at>=SINCE_AT", resolution.width)
		for _, statement := range telemetryMergeSQL(table, aggregate) {
			literal := strings.ReplaceAll(statement, "'", "''")
			literal = strings.ReplaceAll(literal, "SOURCE_SHARD", "',candidate,'")
			literal = strings.ReplaceAll(literal, "SINCE_AT", "',since_at,'")
			body += " SET @telemetry_finalization_sql=CONCAT('" + literal + "');\n PREPARE telemetry_finalization FROM @telemetry_finalization_sql;\n SET statement_open=TRUE;\n EXECUTE telemetry_finalization;\n DEALLOCATE PREPARE telemetry_finalization;\n SET statement_open=FALSE;\n"
		}
	}
	body += ` UPDATE telemetry_sample_shards SET state='retired' WHERE table_name=candidate AND state='active';
 END IF;
 SET @telemetry_finalization_sql=NULL;
END`
	return []LongKeyStep{
		{Name: "telemetry_retire_before_v25", Object: "telemetry_retire_shards", Kind: "procedure", SQL: "DROP PROCEDURE telemetry_retire_shards"},
		// Old retirement receipts did not prove finalization. Requeue any
		// surviving tables; an already-dropped table is only a receipt repair.
		{Name: "telemetry_requeue_unverified_retirement", Object: "telemetry_sample_shards", Kind: "data", Repairable: true,
			SQL:       `UPDATE telemetry_sample_shards s SET state='active' WHERE state='retired' AND EXISTS(SELECT 1 FROM information_schema.tables t WHERE t.table_schema=DATABASE() AND BINARY t.table_name=s.table_name)`,
			VerifySQL: `SELECT IF(NOT EXISTS(SELECT 1 FROM telemetry_sample_shards s WHERE state='retired' AND EXISTS(SELECT 1 FROM information_schema.tables t WHERE t.table_schema=DATABASE() AND BINARY t.table_name=s.table_name)),'valid','invalid')`},
		{Name: "telemetry_finalize_retire_v25", Object: "telemetry_retire_shards", Kind: "procedure", SQL: body},
	}
}
