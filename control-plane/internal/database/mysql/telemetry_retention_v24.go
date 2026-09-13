package mysql

// Append-only authoring input; the existing manifests retain their bytes.
func TelemetryRetentionSteps() []LongKeyStep {
	return []LongKeyStep{
		{Name: "telemetry_5m_retention_index", Object: "telemetry_rollups_5m", Kind: "table", SQL: `ALTER TABLE telemetry_rollups_5m ADD INDEX telemetry_5m_retention_idx(bucket_at,exact_row_id)`},
		{Name: "telemetry_1h_retention_index", Object: "telemetry_rollups_1h", Kind: "table", SQL: `ALTER TABLE telemetry_rollups_1h ADD INDEX telemetry_1h_retention_idx(bucket_at,exact_row_id)`},
		{Name: "telemetry_prune_rollups", Object: "telemetry_prune_rollups", Kind: "procedure", SQL: `CREATE PROCEDURE telemetry_prune_rollups(IN maintenance_time BIGINT)
SQL SECURITY DEFINER
BEGIN
 DECLARE clock_at BIGINT;
 DECLARE cut_hour BIGINT;
 SET clock_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6));
 IF maintenance_time IS NULL OR maintenance_time<clock_at-300000000 OR maintenance_time>clock_at+300000000 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry maintenance clock outside permitted window';
 END IF;
 SET maintenance_time=LEAST(maintenance_time,clock_at);
 SET cut_hour=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',DATE_SUB(TIMESTAMPADD(MICROSECOND,maintenance_time,'2000-01-01'),INTERVAL 13 MONTH));
 DELETE FROM telemetry_rollups_5m WHERE bucket_at<maintenance_time-7776000000000 ORDER BY bucket_at,exact_row_id LIMIT 1000;
 DELETE FROM telemetry_rollups_1h WHERE bucket_at<cut_hour ORDER BY bucket_at,exact_row_id LIMIT 1000;
END`},
		{Name: "telemetry_retire_before_v24", Object: "telemetry_retire_shards", Kind: "procedure", SQL: `DROP PROCEDURE telemetry_retire_shards`},
		{Name: "telemetry_retire_bounded_v24", Object: "telemetry_retire_shards", Kind: "procedure", SQL: `CREATE PROCEDURE telemetry_retire_shards(IN cutoff BIGINT)
SQL SECURITY DEFINER
BEGIN
 DECLARE clock_at BIGINT;
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 SET clock_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6));
 IF cutoff IS NULL OR cutoff>clock_at-1209600000000+300000000 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry retention cutoff outside permitted window';
 END IF;
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF guard_key IS NULL OR migration_state IS NULL OR migration_state<>'complete' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry history migration is incomplete';
 END IF;
 UPDATE telemetry_sample_shards SET state='retired' WHERE state='active' AND end_at<=LEAST(cutoff,clock_at-1209600000000) ORDER BY start_at LIMIT 1;
END`},
	}
}
