package mysql

import "strings"

// Progress and the fixed-size staging table are definer-only. A page and its
// cursor commit together in the caller's fenced transaction, never in SQL.
func telemetryBatchSteps(engine Engine) []LongKeyStep {
	return []LongKeyStep{
		{Name: "telemetry_progress", Object: "telemetry_maintenance_progress", Kind: "table", SQL: `CREATE TABLE telemetry_maintenance_progress (
 singleton TINYINT PRIMARY KEY CHECK(singleton=1),
 phase TINYINT NOT NULL DEFAULT 0,
 cutoff BIGINT NOT NULL DEFAULT 0,
 candidate VARBINARY(64),
 cursor_at BIGINT,
 cursor_node VARBINARY(16),
 cursor_metric LONGBLOB
) ENGINE=InnoDB`},
		{Name: "telemetry_progress_seed", Object: "telemetry_maintenance_progress", Kind: "data", Repairable: true,
			SQL:       `INSERT INTO telemetry_maintenance_progress(singleton) VALUES(1)`,
			VerifySQL: `SELECT IF(EXISTS(SELECT 1 FROM telemetry_maintenance_progress WHERE singleton=1),'valid','invalid')`},
		{Name: "telemetry_page", Object: "telemetry_maintenance_page", Kind: "table", SQL: `CREATE TABLE telemetry_maintenance_page (
 slot SMALLINT PRIMARY KEY CHECK(slot BETWEEN 1 AND 80),
 node_id VARBINARY(16) NOT NULL, metric LONGBLOB NOT NULL, bucket_at BIGINT NOT NULL,
 sample_count BIGINT NOT NULL, min_value DOUBLE NOT NULL, max_value DOUBLE NOT NULL, avg_value DOUBLE NOT NULL
) ENGINE=InnoDB`},
		{Name: "telemetry_batch_retire_v26", Object: "telemetry_retire_shards", Kind: "procedure", SQL: telemetryBatchDDL(engine)},
	}
}

func telemetryBatchDDL(engine Engine) string {
	// SAVEPOINT is inert in autocommit mode, so its immediate RELEASE rejects
	// that mode without committing or rolling back the caller's transaction.
	// Lock page parents after staging but before the merge statement snapshot;
	// concurrent parent updates must not invalidate MariaDB's FK read.
	body := `CREATE PROCEDURE telemetry_retire_shards(IN requested_cutoff BIGINT)
SQL SECURITY DEFINER
main: BEGIN
 DECLARE clock_at BIGINT;
 DECLARE phase_no INT;
 DECLARE cut_at BIGINT;
 DECLARE since_at BIGINT;
 DECLARE upper_at BIGINT;
 DECLARE stop_at BIGINT;
 DECLARE cursor_at BIGINT;
 DECLARE cursor_node VARBINARY(16);
 DECLARE cursor_metric LONGBLOB;
 DECLARE candidate VARBINARY(64);
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 DECLARE sources LONGTEXT;
 DECLARE aggregate_sql LONGTEXT;
 DECLARE target_table VARCHAR(64);
 DECLARE page_count INT;
 DECLARE statement_open BOOLEAN DEFAULT FALSE;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION
 BEGIN
  IF statement_open THEN DEALLOCATE PREPARE telemetry_batch; END IF;
  SET @telemetry_batch_sql=NULL, @telemetry_batch_matches=NULL;
  RESIGNAL;
 END;
 SET clock_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6));
 IF requested_cutoff IS NULL OR requested_cutoff>clock_at-1209600000000+300000000 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry retention cutoff outside permitted window';
 END IF;
 SAVEPOINT ocservia_telemetry_batch_tx;
 RELEASE SAVEPOINT ocservia_telemetry_batch_tx;
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF guard_key IS NULL OR migration_state IS NULL OR migration_state<>'complete' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry history migration is incomplete';
 END IF;
 SELECT phase,p.cutoff,p.candidate,p.cursor_at,p.cursor_node,p.cursor_metric
 INTO phase_no,cut_at,candidate,cursor_at,cursor_node,cursor_metric
 FROM telemetry_maintenance_progress p WHERE singleton=1 FOR UPDATE;
 IF phase_no=0 THEN
  SET cut_at=LEAST(requested_cutoff,clock_at-1209600000000);
  SET candidate=NULL;
  SELECT table_name INTO candidate FROM telemetry_sample_shards FORCE INDEX (telemetry_retirement_lookup)
  WHERE state='active' AND end_at<=cut_at ORDER BY start_at LIMIT 1;
  SET phase_no=1, cursor_at=NULL, cursor_node=NULL, cursor_metric=NULL;
  UPDATE telemetry_maintenance_progress p SET phase=1,cutoff=cut_at,p.candidate=candidate,
   cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
 END IF;
 phases: WHILE phase_no<=4 DO
  IF phase_no>=3 AND candidate IS NULL THEN LEAVE phases; END IF;
  SET target_table=IF(MOD(phase_no,2)=1,'telemetry_rollups_5m','telemetry_rollups_1h');
  IF phase_no<=2 THEN
   SET since_at=FLOOR(GREATEST(cut_at,clock_at-1209600000000)/IF(phase_no=1,300000000,3600000000))*IF(phase_no=1,300000000,3600000000);
  ELSEIF phase_no=3 THEN
   SET since_at=FLOOR((clock_at-7776000000000)/300000000)*300000000;
  ELSE
   SET since_at=FLOOR(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 13 MONTH))/3600000000)*3600000000;
  END IF;
  IF cursor_at IS NOT NULL THEN SET since_at=GREATEST(since_at,cursor_at); END IF;
  SET stop_at=GREATEST(cut_at+1209600000000,clock_at)+300000000;
  IF phase_no>=3 THEN SELECT GREATEST(since_at,start_at),end_at INTO since_at,stop_at FROM telemetry_sample_shards WHERE table_name=candidate; END IF;
  SET upper_at=9223372036854775807;
  IF since_at<stop_at-86400000000 THEN SET upper_at=since_at+86400000000; END IF;
  SET sources='';
  IF phase_no<=2 THEN
   SET sources=CONCAT('SELECT node_id,metric,sampled_at,value FROM telemetry_samples WHERE sampled_at>=',since_at,' AND sampled_at<=',upper_at,IF(upper_at=9223372036854775807,'','-1'));
   BEGIN
    DECLARE finished BOOLEAN DEFAULT FALSE;
    DECLARE shard VARBINARY(64);
    DECLARE shards CURSOR FOR SELECT table_name FROM telemetry_sample_shards WHERE state='active' AND end_at>since_at ORDER BY start_at;
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET finished=TRUE;
    OPEN shards;
    shard_loop: LOOP
     FETCH shards INTO shard;
     IF finished THEN LEAVE shard_loop; END IF;
     IF CONVERT(shard USING ascii) NOT REGEXP '^telemetry_samples_(m_[0-9]{6}|x_[pn][0-9]{7})$' THEN
      SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='invalid telemetry shard name';
     END IF;
     SET sources=CONCAT(sources,' UNION ALL SELECT node_id,metric,sampled_at,value FROM ',shard,' WHERE sampled_at>=',since_at,' AND sampled_at<=',upper_at,IF(upper_at=9223372036854775807,'','-1'));
    END LOOP;
    CLOSE shards;
   END;
  ELSE
   IF CONVERT(candidate USING ascii) NOT REGEXP '^telemetry_samples_(m_[0-9]{6}|x_[pn][0-9]{7})$' THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='invalid telemetry shard name';
   END IF;
   SET sources=CONCAT('SELECT node_id,metric,sampled_at,value FROM ',candidate,' WHERE sampled_at>=',since_at,' AND sampled_at<=',upper_at,IF(upper_at=9223372036854775807,'','-1'));
  END IF;
  SET aggregate_sql=IF(MOD(phase_no,2)=1,CONCAT('AGGREGATE_5M_PREFIX',sources,'AGGREGATE_5M_SUFFIX'),CONCAT('AGGREGATE_1H_PREFIX',sources,'AGGREGATE_1H_SUFFIX'));
  DELETE FROM telemetry_maintenance_page;
  SET @telemetry_batch_sql=CONCAT('INSERT INTO telemetry_maintenance_page SELECT ROW_NUMBER() OVER(ORDER BY bucket_at,node_id,BINARY metric),page.* FROM (SELECT a.* FROM (',aggregate_sql,') a LEFT JOIN ',target_table,
   ' r ON r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at WHERE (r.exact_row_id IS NULL OR NOT(r.sample_count <=> a.sample_count AND r.min_value <=> a.min_value AND r.max_value <=> a.max_value AND r.avg_value <=> a.avg_value))');
  IF cursor_node IS NOT NULL THEN
   SET @telemetry_batch_sql=CONCAT(@telemetry_batch_sql,' AND (a.bucket_at>',cursor_at,
    ' OR (a.bucket_at=',cursor_at,' AND (a.node_id>UNHEX(''',HEX(cursor_node),''')',
    ' OR (a.node_id=UNHEX(''',HEX(cursor_node),''') AND BINARY a.metric>UNHEX(''',HEX(cursor_metric),''')))))');
  END IF;
  SET @telemetry_batch_sql=CONCAT(@telemetry_batch_sql,' ORDER BY a.bucket_at,a.node_id,BINARY a.metric LIMIT 80) page');
  PREPARE telemetry_batch FROM @telemetry_batch_sql;
  SET statement_open=TRUE;
  EXECUTE telemetry_batch;
  DEALLOCATE PREPARE telemetry_batch;
  SET statement_open=FALSE;
  SELECT COUNT(*) INTO page_count FROM telemetry_maintenance_page;
  IF page_count>0 THEN
   LOCK_PAGE_PARENTS
   SET guard_key=NULL;
   SELECT key_name INTO guard_key FROM exact_key_guards WHERE key_name=target_table FOR UPDATE;
   IF guard_key IS NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='missing exact-key guard'; END IF;
   MERGE_PAGE
   SELECT p.bucket_at,p.node_id,p.metric INTO cursor_at,cursor_node,cursor_metric FROM telemetry_maintenance_page p ORDER BY slot DESC LIMIT 1;
   UPDATE telemetry_maintenance_progress p SET p.cursor_at=cursor_at,p.cursor_node=cursor_node,p.cursor_metric=cursor_metric WHERE singleton=1;
   SET @telemetry_batch_sql=NULL;
   SELECT FALSE AS done;
   LEAVE main;
  END IF;
  IF upper_at<stop_at THEN
   SET cursor_at=upper_at,cursor_node=NULL,cursor_metric=NULL;
   UPDATE telemetry_maintenance_progress p SET p.cursor_at=cursor_at,p.cursor_node=NULL,p.cursor_metric=NULL WHERE singleton=1;
   ITERATE phases;
  END IF;
  SET phase_no=phase_no+1,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL;
  UPDATE telemetry_maintenance_progress SET phase=phase_no,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
 END WHILE;
 IF candidate IS NOT NULL THEN
  SELECT table_name INTO guard_key FROM telemetry_sample_shards WHERE table_name=candidate AND state='active' FOR UPDATE;
  VERIFY_FINALIZATION
  UPDATE telemetry_sample_shards SET state='retired' WHERE table_name=candidate AND state='active';
 END IF;
 UPDATE telemetry_maintenance_progress SET phase=0,candidate=NULL,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
 DELETE FROM telemetry_maintenance_page;
 SET @telemetry_batch_sql=NULL,@telemetry_batch_matches=NULL;
 SELECT TRUE AS done;
END`
	parents := ""
	if engine == MariaDB {
		parents = `BEGIN
    DECLARE finished BOOLEAN DEFAULT FALSE;
    DECLARE parent_id VARBINARY(16);
    DECLARE locked_id VARBINARY(16);
    DECLARE parents CURSOR FOR SELECT DISTINCT node_id FROM telemetry_maintenance_page ORDER BY node_id;
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET finished=TRUE;
    OPEN parents;
    parent_loop: LOOP
     FETCH parents INTO parent_id;
     IF finished THEN LEAVE parent_loop; END IF;
     SELECT id INTO locked_id FROM nodes WHERE id=parent_id LOCK IN SHARE MODE;
    END LOOP;
    CLOSE parents;
   END;`
	}
	body = strings.Replace(body, "LOCK_PAGE_PARENTS", parents, 1)
	for _, r := range []struct {
		name  string
		width int64
	}{{"5M", 300000000}, {"1H", 3600000000}} {
		parts := strings.Split(telemetryAggregateSQL("SOURCE", r.width), "SOURCE")
		body = strings.ReplaceAll(body, "AGGREGATE_"+r.name+"_PREFIX", strings.ReplaceAll(parts[0], "'", "''"))
		body = strings.ReplaceAll(body, "AGGREGATE_"+r.name+"_SUFFIX", strings.ReplaceAll(parts[1], "'", "''"))
	}
	shortMerge := `INSERT INTO TARGET(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value FROM telemetry_maintenance_page WHERE TRUE ON DUPLICATE KEY UPDATE sample_count=VALUES(sample_count),min_value=VALUES(min_value),max_value=VALUES(max_value),avg_value=VALUES(avg_value)`
	merge := "IF NOT EXISTS(SELECT 1 FROM telemetry_maintenance_page WHERE OCTET_LENGTH(metric)>255) THEN\n" + batchExecute("CONCAT('"+strings.ReplaceAll(shortMerge, "TARGET", "',target_table,'")+"')") + "ELSE\n"
	for _, sql := range telemetryMergeSQL("TARGET", `SELECT node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value FROM telemetry_maintenance_page`) {
		merge += batchExecute("CONCAT('" + strings.ReplaceAll(strings.ReplaceAll(sql, "'", "''"), "TARGET", "',target_table,'") + "')")
	}
	merge += "END IF;\n"
	body = strings.Replace(body, "MERGE_PAGE", merge, 1)
	verify := ""
	for _, r := range []struct {
		suffix, cutoff string
		width          int64
	}{
		{"5m", "FLOOR((clock_at-7776000000000)/300000000)*300000000", 300000000},
		{"1h", "FLOOR(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 13 MONTH))/3600000000)*3600000000", 3600000000},
	} {
		aggregate := telemetryAggregateSQL("SELECT node_id,metric,sampled_at,value FROM `SOURCE` WHERE sampled_at>=SINCE", r.width)
		check := "SELECT NOT EXISTS(SELECT 1 FROM (" + aggregate + ") a LEFT JOIN telemetry_rollups_" + r.suffix + " r ON r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at WHERE r.exact_row_id IS NULL OR NOT(r.sample_count <=> a.sample_count AND r.min_value <=> a.min_value AND r.max_value <=> a.max_value AND r.avg_value <=> a.avg_value)) INTO @telemetry_batch_matches"
		check = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(check, "'", "''"), "SOURCE", "',candidate,'"), "SINCE", "',since_at,'")
		verify += "SET since_at=" + r.cutoff + ";\n" + batchExecute("CONCAT('"+check+"')") + `
  IF NOT @telemetry_batch_matches THEN
   UPDATE telemetry_maintenance_progress SET phase=3,cursor_at=NULL,cursor_node=NULL,cursor_metric=NULL WHERE singleton=1;
   SET @telemetry_batch_sql=NULL,@telemetry_batch_matches=NULL;
   SELECT FALSE AS done;
   LEAVE main;
  END IF;
`
	}
	return strings.Replace(body, "VERIFY_FINALIZATION", verify, 1)
}

func batchExecute(expression string) string {
	return "SET @telemetry_batch_sql=" + expression + ";\nPREPARE telemetry_batch FROM @telemetry_batch_sql;\nSET statement_open=TRUE;\nEXECUTE telemetry_batch;\nDEALLOCATE PREPARE telemetry_batch;\nSET statement_open=FALSE;\n"
}
