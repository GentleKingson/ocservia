package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

const TelemetryShardCatalogDDL = `CREATE TABLE telemetry_sample_shards (
 table_name VARBINARY(64) NOT NULL PRIMARY KEY,
 start_at BIGINT NOT NULL UNIQUE,
 end_at BIGINT NOT NULL,
 state VARBINARY(8) NOT NULL CHECK(state IN ('planned','active','retired','dropped')),
 CHECK(end_at>start_at)
) ENGINE=InnoDB`

// Runtime receives EXECUTE only. No DDL and no catalog UPDATE privilege is
// granted to runtime; the bounded retirement participates in its caller's Tx.
const TelemetryRetireShardsDDL = `CREATE PROCEDURE telemetry_retire_shards(IN cutoff BIGINT)
SQL SECURITY DEFINER
BEGIN
 DECLARE clock_at BIGINT;
 DECLARE guard_key VARBINARY(128);
 SET clock_at = TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6));
 IF cutoff IS NULL OR cutoff < clock_at-7776000000000 OR cutoff > clock_at THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry retention cutoff outside permitted window';
 END IF;
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 IF guard_key IS NULL THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry catalog guard is unavailable';
 END IF;
 UPDATE telemetry_sample_shards SET state='retired' WHERE state='active' AND end_at<=cutoff;
END`

// GrantTelemetryTestPrivileges is called by the owner after provisioning.
// Runtime gets immutable history access and the bounded retention procedure;
// neither runtime nor maintenance receives catalog writes or arbitrary DDL.
func (b *Backend) GrantTelemetryTestPrivileges(ctx context.Context) error {
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		return safeError(err)
	}
	defer conn.Close()
	names, err := validateTelemetryShards(ctx, conn)
	if err != nil {
		return err
	}
	var schema string
	if err = conn.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&schema); err != nil {
		return safeError(err)
	}
	if !identifier.MatchString(schema) {
		return ErrSchema
	}
	for _, account := range []string{"ocservia_app", "ocservia_maintenance"} {
		for _, grant := range []string{"GRANT SELECT ON `" + schema + "`.`telemetry_sample_shards`", "GRANT SELECT ON `" + schema + "`.`business_locks`", "GRANT EXECUTE ON PROCEDURE `" + schema + "`.`telemetry_retire_shards`"} {
			if _, err = conn.ExecContext(ctx, grant+" TO '"+account+"'@'%'"); err != nil {
				return safeError(err)
			}
		}
		for _, name := range names {
			privileges := "SELECT"
			if account == "ocservia_app" {
				privileges = "SELECT,INSERT"
			}
			if _, err = conn.ExecContext(ctx, "GRANT "+privileges+" ON `"+schema+"`.`"+name+"` TO '"+account+"'@'%'"); err != nil {
				return safeError(err)
			}
		}
	}
	return nil
}

// The migration pins this template's SHOW CREATE hash. Every dynamic shard is
// checked against that same pinned definition after substituting only its
// name and month bounds; owner-supplied catalog text is never trusted as SQL.
func TelemetryShardTemplateDDL(collation string) (string, error) {
	if collation != "utf8mb4_0900_bin" && collation != "utf8mb4_nopad_bin" {
		return "", ErrSchema
	}
	return telemetryShardDDL("telemetry_samples_template", value.MinTimestamp, value.EndTimestamp, collation), nil
}

func telemetryShardDDL(name string, start, end int64, collation string) string {
	return fmt.Sprintf(`CREATE TABLE %s (
 node_id VARBINARY(16) NOT NULL CHECK(OCTET_LENGTH(node_id)=16),
 batch_id VARBINARY(16) NOT NULL CHECK(OCTET_LENGTH(batch_id)=16),
 sampled_at BIGINT NOT NULL,
 metric VARCHAR(32) NOT NULL,
 value DOUBLE NOT NULL,
 PRIMARY KEY(sampled_at,node_id,batch_id,metric),
 KEY %s_query_idx(node_id,metric,sampled_at DESC),
 CONSTRAINT %s_node_fk FOREIGN KEY(node_id) REFERENCES nodes(id) ON DELETE RESTRICT,
 CONSTRAINT %s_batch_fk FOREIGN KEY(batch_id) REFERENCES telemetry_ingest_batches(batch_id) ON DELETE CASCADE,
 CONSTRAINT %s_metric CHECK(metric IN ('cpu_usage_ratio','memory_used_bytes','network_rx_bytes','network_tx_bytes','session_count','connection_rtt_ms')),
 CHECK(value BETWEEN -1.7976931348623157e308 AND 1.7976931348623157e308),
 CHECK(LOCATE(0x00,CAST(metric AS BINARY))=0),
 CONSTRAINT %s_month CHECK(sampled_at>=%d AND sampled_at<%d)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=%s`, name, name, name, name, name, name, start, end, collation)
}

func telemetryMonth(month time.Time) (string, int64, int64, error) {
	month = month.UTC()
	if month.Year() < 1 || month.Year() > 9999 {
		return "", 0, 0, ErrSchema
	}
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	lo, err := telemetryMicros(start)
	if err != nil {
		return "", 0, 0, err
	}
	hi, err := telemetryMicros(start.AddDate(0, 1, 0))
	if err != nil {
		return "", 0, 0, err
	}
	return "telemetry_samples_m_" + start.Format("200601"), lo, hi, nil
}

func shardDefinition(ctx context.Context, conn *sql.Conn, name string) (string, error) {
	var actual, definition string
	if err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE `"+name+"`").Scan(&actual, &definition); err != nil {
		return "", safeError(err)
	}
	if actual != name {
		return "", ErrSchema
	}
	return definition, nil
}

func checkTelemetryShard(ctx context.Context, conn *sql.Conn, name string, start, end int64) error {
	if !telemetryShardName.MatchString(name) {
		return ErrSchema
	}
	month, err := time.Parse("200601", strings.TrimPrefix(name, "telemetry_samples_m_"))
	if err != nil {
		return ErrSchema
	}
	expected, lo, hi, err := telemetryMonth(month)
	if err != nil || expected != name || lo != start || hi != end {
		return ErrSchema
	}
	template, err := shardDefinition(ctx, conn, "telemetry_samples_template")
	if err != nil {
		return err
	}
	actual, err := shardDefinition(ctx, conn, name)
	if err != nil {
		return err
	}
	actual, err = normalizeTelemetryShard(actual, name, start, end)
	if err != nil {
		return err
	}
	template, err = normalizeTelemetryShard(template, "telemetry_samples_template", value.MinTimestamp, value.EndTimestamp)
	if err != nil {
		return err
	}
	if actual != template {
		return ErrSchema
	}
	return nil
}

func normalizeTelemetryShard(definition, name string, start, end int64) (string, error) {
	definition = strings.ReplaceAll(definition, name, "telemetry_samples_template")
	for _, bound := range []struct {
		operator, marker string
		n                int64
	}{{">=", "@month_start", start}, {"<", "@month_end", end}} {
		prefix := "`sampled_at` " + bound.operator + " "
		literal := fmt.Sprint(bound.n)
		if bound.n < 0 && strings.Contains(definition, prefix+"-("+strings.TrimPrefix(literal, "-")+")") {
			literal = "-(" + strings.TrimPrefix(literal, "-") + ")"
		}
		if strings.Count(definition, prefix+literal) != 1 {
			return "", ErrSchema
		}
		definition = strings.Replace(definition, prefix+literal, prefix+bound.marker, 1)
	}
	return definition, nil
}

// validateTelemetryShards returns only verified physical shard names. The
// migration validator must count these alongside static manifest tables, not
// exempt an arbitrary prefix from schema validation.
func validateTelemetryShards(ctx context.Context, conn *sql.Conn) ([]string, error) {
	var guards int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM business_locks WHERE lock_key='telemetry-shard-catalog'`).Scan(&guards); err != nil {
		return nil, safeError(err)
	}
	if guards != 1 {
		return nil, ErrSchema
	}
	rows, err := conn.QueryContext(ctx, `SELECT table_name,start_at,end_at,state FROM telemetry_sample_shards ORDER BY start_at`)
	if err != nil {
		return nil, safeError(err)
	}
	type shard struct {
		name, state string
		start, end  int64
	}
	var shards []shard
	for rows.Next() {
		var s shard
		if err = rows.Scan(&s.name, &s.start, &s.end, &s.state); err != nil {
			rows.Close()
			return nil, safeError(err)
		}
		shards = append(shards, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, safeError(err)
	}
	var names []string
	for i, s := range shards {
		if !telemetryShardName.MatchString(s.name) || i > 0 && s.start < shards[i-1].end {
			return nil, ErrSchema
		}
		month, err := time.Parse("200601", strings.TrimPrefix(s.name, "telemetry_samples_m_"))
		if err != nil {
			return nil, ErrSchema
		}
		name, start, end, err := telemetryMonth(month)
		if err != nil || name != s.name || start != s.start || end != s.end {
			return nil, ErrSchema
		}
		if s.state == "planned" {
			return nil, ErrDirty
		}
		var exists int
		if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, s.name).Scan(&exists); err != nil {
			return nil, safeError(err)
		}
		switch s.state {
		case "dropped":
			if exists != 0 {
				return nil, ErrSchema
			}
			continue
		case "retired":
			if exists == 0 {
				continue
			}
		case "active":
			if exists == 0 {
				return nil, ErrSchema
			}
		default:
			return nil, ErrSchema
		}
		if err = checkTelemetryShard(ctx, conn, s.name, s.start, s.end); err != nil {
			return nil, err
		}
		names = append(names, s.name)
	}
	return names, nil
}

// ProvisionTelemetryMonth is owner-only. A planned receipt is durable before
// DDL; rerunning it verifies an already-created table, never silently adopting
// an unknown object. Activation is last, so runtime never sees partial DDL.
func (b *Backend) ProvisionTelemetryMonth(ctx context.Context, month time.Time) (result error) {
	name, start, end, err := telemetryMonth(month)
	if err != nil {
		return err
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, lock)) }()
	var state string
	err = conn.QueryRowContext(ctx, `SELECT state FROM telemetry_sample_shards WHERE table_name=?`, name).Scan(&state)
	if err == sql.ErrNoRows {
		var exists int
		if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, name).Scan(&exists); err != nil {
			return safeError(err)
		}
		if exists != 0 {
			return ErrSchema
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO telemetry_sample_shards(table_name,start_at,end_at,state) VALUES(?,?,?,'planned')`, name, start, end); err != nil {
			return safeError(err)
		}
		state = "planned"
	} else if err != nil {
		return safeError(err)
	}
	if state != "planned" && state != "active" {
		return ErrSchema
	}
	var exists int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, name).Scan(&exists); err != nil {
		return safeError(err)
	}
	if exists == 0 {
		if state != "planned" {
			return ErrSchema
		}
		collation := "utf8mb4_0900_bin"
		if b.engine == MariaDB {
			collation = "utf8mb4_nopad_bin"
		}
		if _, err = conn.ExecContext(ctx, telemetryShardDDL(name, start, end, collation)); err != nil {
			return safeError(err)
		}
	}
	if err = checkTelemetryShard(ctx, conn, name, start, end); err != nil {
		return err
	}
	if state == "active" {
		return nil
	}
	return activateTelemetryShard(ctx, conn, name, start, end)
}

// Owner DDL has finished before this transaction starts. Copy, exact row-count
// checks, source deletion and catalog activation commit together. The fixed
// catalog guard also covers readers which selected their shard list before
// this month was planned, avoiding a READ COMMITTED gap during legacy moves.
func activateTelemetryShard(ctx context.Context, conn *sql.Conn, name string, start, end int64) (result error) {
	if _, err := conn.ExecContext(ctx, `SET TRANSACTION ISOLATION LEVEL SERIALIZABLE`); err != nil {
		return safeError(err)
	}
	if _, err := conn.ExecContext(ctx, `START TRANSACTION`); err != nil {
		return safeError(err)
	}
	active := true
	defer func() {
		if active {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(cleanup, `ROLLBACK`); err != nil {
				discard(conn)
				result = errors.Join(result, safeError(err))
			}
		}
	}()
	var guard []byte
	if err := conn.QueryRowContext(ctx, `SELECT lock_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' FOR UPDATE`).Scan(&guard); err != nil {
		return safeError(err)
	}
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM telemetry_sample_shards WHERE table_name=? AND start_at=? AND end_at=? FOR UPDATE`, name, start, end).Scan(&state); err != nil {
		return safeError(err)
	}
	if state != "planned" {
		return ErrSchema
	}
	var count int64
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+name).Scan(&count); err != nil {
		return safeError(err)
	}
	if count != 0 {
		return ErrSchema
	}
	rows, err := conn.QueryContext(ctx, `SELECT sampled_at FROM telemetry_samples WHERE sampled_at>=? AND sampled_at<? FOR UPDATE`, start, end)
	if err != nil {
		return safeError(err)
	}
	for rows.Next() {
		var at int64
		if err = rows.Scan(&at); err != nil {
			rows.Close()
			return safeError(err)
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return safeError(err)
	}
	inserted, err := conn.ExecContext(ctx, `INSERT INTO `+name+`(node_id,batch_id,sampled_at,metric,value) SELECT node_id,batch_id,sampled_at,metric,value FROM telemetry_samples WHERE sampled_at>=? AND sampled_at<?`, start, end)
	if err != nil {
		return safeError(err)
	}
	n, err := inserted.RowsAffected()
	if err != nil {
		return safeError(err)
	}
	if n != count {
		return ErrSchema
	}
	var matches int64
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM telemetry_samples AS source JOIN `+name+` AS target ON source.sampled_at=target.sampled_at AND source.node_id=target.node_id AND source.batch_id=target.batch_id AND BINARY source.metric=BINARY target.metric AND source.value=target.value WHERE source.sampled_at>=? AND source.sampled_at<?`, start, end).Scan(&matches); err != nil {
		return safeError(err)
	}
	if matches != count {
		return ErrSchema
	}
	deleted, err := conn.ExecContext(ctx, `DELETE FROM telemetry_samples WHERE sampled_at>=? AND sampled_at<?`, start, end)
	if err != nil {
		return safeError(err)
	}
	n, err = deleted.RowsAffected()
	if err != nil {
		return safeError(err)
	}
	if n != count {
		return ErrSchema
	}
	if _, err = conn.ExecContext(ctx, `UPDATE telemetry_sample_shards SET state='active' WHERE table_name=? AND state='planned'`, name); err != nil {
		return safeError(err)
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return safeError(err)
	}
	active = false
	return nil
}

// CollectRetiredTelemetryShards is deliberately not part of a business Tx:
// MySQL DDL commits implicitly. Only committed retirement makes a table
// eligible; active readers/writers lock that receipt until their Tx ends.
func (b *Backend) CollectRetiredTelemetryShards(ctx context.Context) (result error) {
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, lock)) }()
	rows, err := conn.QueryContext(ctx, `SELECT table_name,start_at,end_at FROM telemetry_sample_shards WHERE state='retired' ORDER BY start_at`)
	if err != nil {
		return safeError(err)
	}
	type shard struct {
		name       string
		start, end int64
	}
	var shards []shard
	for rows.Next() {
		var s shard
		if err = rows.Scan(&s.name, &s.start, &s.end); err != nil {
			rows.Close()
			return safeError(err)
		}
		shards = append(shards, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return safeError(err)
	}
	for _, s := range shards {
		var exists int
		if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, s.name).Scan(&exists); err != nil {
			return safeError(err)
		}
		if exists != 0 {
			if err = checkTelemetryShard(ctx, conn, s.name, s.start, s.end); err != nil {
				return err
			}
			if _, err = conn.ExecContext(ctx, "DROP TABLE `"+s.name+"`"); err != nil {
				return safeError(err)
			}
		}
		if _, err = conn.ExecContext(ctx, `UPDATE telemetry_sample_shards SET state='dropped' WHERE table_name=? AND state='retired'`, s.name); err != nil {
			return safeError(err)
		}
	}
	return nil
}
