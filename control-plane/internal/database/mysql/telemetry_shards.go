package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
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
	if err := b.grantTelemetryPrivileges(ctx, "'ocservia_app'@'%'", true); err != nil {
		return err
	}
	return b.grantTelemetryPrivileges(ctx, "'ocservia_maintenance'@'%'", false)
}

// PrepareControllerTelemetry is owner-only startup work. Runtime ingestion
// accepts the last 14 days, so provision that window and two future months.
// Re-running --migrate-only advances this horizon without granting runtime DDL.
func (b *Backend) PrepareControllerTelemetry(ctx context.Context) error {
	if err := b.MigrateTelemetryHistory(ctx); err != nil {
		return err
	}
	var now time.Time
	if err := b.QueryRow(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&now); err != nil {
		return err
	}
	oldest := now.Add(-14 * 24 * time.Hour)
	start := time.Date(oldest.Year(), oldest.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(now.Year(), now.Month()+3, 1, 0, 0, 0, 0, time.UTC)
	for month := start; month.Before(end); month = month.AddDate(0, 1, 0) {
		if err := b.ProvisionTelemetryMonth(ctx, month); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTelemetryRuntime never provisions or repairs storage. Every month
// accepted by ingestion must already be active and readable by this account.
func (b *Backend) ValidateTelemetryRuntime(ctx context.Context) error {
	grants, err := b.Query(ctx, `SHOW GRANTS FOR CURRENT_USER`)
	if err != nil {
		return err
	}
	for grants.Next() {
		var grant string
		if err := grants.Scan(&grant); err != nil {
			grants.Close()
			return err
		}
		prefix, rest, direct := strings.Cut(strings.ToUpper(grant), " ON ")
		object, _, _ := strings.Cut(rest, " TO ")
		unsafe := !direct || strings.Contains(rest, "WITH GRANT OPTION") || (strings.HasPrefix(rest, "*.* ") && prefix != "GRANT USAGE")
		for _, privilege := range strings.Split(strings.TrimPrefix(prefix, "GRANT "), ",") {
			words := strings.Fields(privilege)
			if len(words) == 0 {
				continue
			}
			switch words[0] {
			case "ALL", "CREATE", "ALTER", "DROP", "TRIGGER", "EVENT":
				unsafe = true
			case "DELETE":
				// Schema grants may not refresh on an already-open connection;
				// inspect their scope as well as probing effective table access.
				unsafe = unsafe || strings.HasSuffix(object, ".*")
			}
		}
		if unsafe {
			grants.Close()
			return fmt.Errorf("runtime grants contain unrestricted authority or role inheritance: %w", ErrSchema)
		}
	}
	err = grants.Err()
	grants.Close()
	if err != nil {
		return err
	}
	for _, table := range []string{"telemetry_rollups_5m", "telemetry_rollups_1h"} {
		// A false predicate checks effective privileges, including inherited
		// global/schema grants, without deleting a row or relying on grant text.
		_, err := b.Exec(ctx, `DELETE FROM `+table+` WHERE 1=0`)
		if !errors.Is(err, database.ErrPermission) {
			return fmt.Errorf("runtime must not have unrestricted telemetry DELETE privileges: %w", ErrSchema)
		}
	}
	for _, routine := range []string{"telemetry_prune_rollups", "telemetry_retire_shards"} {
		// NULL is rejected by each pinned routine before locks or writes. The
		// expected signal proves EXECUTE is available without running cleanup.
		_, err := b.pool.ExecContext(ctx, `CALL `+routine+`(NULL)`)
		var serverError *driver.MySQLError
		if !errors.As(err, &serverError) || serverError.Number != 1644 || string(serverError.SQLState[:]) != "45000" {
			return fmt.Errorf("required telemetry maintenance routine is unavailable: %w", ErrSchema)
		}
	}
	if err := b.ValidateTelemetryHistoryReady(ctx); err != nil {
		return err
	}
	var now time.Time
	if err := b.QueryRow(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&now); err != nil {
		return err
	}
	oldest := now.Add(-14 * 24 * time.Hour)
	start := time.Date(oldest.Year(), oldest.Month(), 1, 0, 0, 0, 0, time.UTC)
	for month := start; !month.After(now.Add(5 * time.Minute)); month = month.AddDate(0, 1, 0) {
		name, lo, hi, err := telemetryMonth(month)
		if err != nil {
			return err
		}
		var active bool
		if err := b.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telemetry_sample_shards WHERE table_name=? AND start_at=? AND end_at=? AND state='active')`, name, lo, hi).Scan(&active); err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("required telemetry month %s is not provisioned: %w", month.Format("2006-01"), ErrSchema)
		}
		rows, err := b.Query(ctx, `SELECT node_id,batch_id,sampled_at,metric,value FROM `+name+` LIMIT 0`)
		if err != nil {
			return err
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) grantTelemetryPrivileges(ctx context.Context, quotedAccount string, runtime bool) error {
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
	for _, grant := range []string{"GRANT SELECT ON `" + schema + "`.`telemetry_sample_shards`", "GRANT SELECT ON `" + schema + "`.`business_locks`", "GRANT EXECUTE ON PROCEDURE `" + schema + "`.`telemetry_retire_shards`"} {
		if _, err = conn.ExecContext(ctx, grant+" TO "+quotedAccount); err != nil {
			return safeError(err)
		}
	}
	for _, name := range names {
		privileges := "SELECT"
		if runtime {
			privileges = "SELECT,INSERT"
		}
		if _, err = conn.ExecContext(ctx, "GRANT "+privileges+" ON `"+schema+"`.`"+name+"` TO "+quotedAccount); err != nil {
			return safeError(err)
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
	if month.Year() < -4713 || month.Year() > 294276 {
		return "", 0, 0, ErrSchema
	}
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Only the first and final PostgreSQL timestamp months may have clipped
	// bounds. Unix seconds, unlike UnixNano, cover the complete finite range.
	lo := (start.Unix() - 946684800) * 1000000
	hi := (start.AddDate(0, 1, 0).Unix() - 946684800) * 1000000
	if lo < value.MinTimestamp {
		lo = value.MinTimestamp
	}
	if hi > value.EndTimestamp {
		hi = value.EndTimestamp
	}
	if lo >= hi || lo >= value.EndTimestamp || hi <= value.MinTimestamp {
		return "", 0, 0, ErrSchema
	}
	name := "telemetry_samples_m_" + start.Format("200601")
	if month.Year() < 1 || month.Year() > 9999 {
		ordinal := month.Year()*12 + int(month.Month()) - 1
		sign := "p"
		if ordinal < 0 {
			sign = "n"
			ordinal = -ordinal
		}
		name = fmt.Sprintf("telemetry_samples_x_%s%07d", sign, ordinal)
	}
	return name, lo, hi, nil
}

func telemetryMonthFromName(name string) (time.Time, error) {
	if !telemetryShardName.MatchString(name) {
		return time.Time{}, ErrSchema
	}
	if strings.HasPrefix(name, "telemetry_samples_m_") {
		return time.Parse("200601", strings.TrimPrefix(name, "telemetry_samples_m_"))
	}
	encoded := strings.TrimPrefix(name, "telemetry_samples_x_")
	ordinal, err := strconv.Atoi(encoded[1:])
	if err != nil {
		return time.Time{}, ErrSchema
	}
	if encoded[0] == 'n' {
		ordinal = -ordinal
	}
	year, remainder := ordinal/12, ordinal%12
	if remainder < 0 {
		year--
		remainder += 12
	}
	month := time.Date(year, time.Month(remainder+1), 1, 0, 0, 0, 0, time.UTC)
	canonical, _, _, err := telemetryMonth(month)
	if err != nil || canonical != name {
		return time.Time{}, ErrSchema
	}
	return month, nil
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
	month, err := telemetryMonthFromName(name)
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
		month, err := telemetryMonthFromName(s.name)
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
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, lock)) }()
	return b.provisionTelemetryMonth(ctx, conn, month)
}

func (b *Backend) provisionTelemetryMonth(ctx context.Context, conn *sql.Conn, month time.Time) error {
	name, start, end, err := telemetryMonth(month)
	if err != nil {
		return err
	}
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
	if state != "planned" && state != "active" {
		return ErrSchema
	}
	var count int64
	if state == "planned" {
		var existing int64
		err := conn.QueryRowContext(ctx, `SELECT sampled_at FROM `+name+` LIMIT 1 FOR UPDATE NOWAIT`).Scan(&existing)
		if err == nil {
			return ErrSchema
		}
		if err != sql.ErrNoRows {
			return safeError(err)
		}
	}
	count = 0
	rows, err := conn.QueryContext(ctx, `SELECT sampled_at,node_id,batch_id FROM telemetry_samples WHERE sampled_at>=? AND sampled_at<? FOR UPDATE NOWAIT`, start, end)
	if err != nil {
		return safeError(err)
	}
	nodes := map[string]struct{}{}
	batches := map[string]struct{}{}
	for rows.Next() {
		var at int64
		var node, batch []byte
		if err = rows.Scan(&at, &node, &batch); err != nil {
			rows.Close()
			return safeError(err)
		}
		nodes[string(node)] = struct{}{}
		batches[string(batch)] = struct{}{}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return safeError(err)
	}
	// Do not wait on business node locks while holding the catalog guard.
	// NOWAIT preserves the existing node-first business order without retrying
	// or partially moving a busy month's data.
	rows, err = conn.QueryContext(ctx, `SELECT sampled_at FROM `+name+` WHERE sampled_at>=? AND sampled_at<? FOR UPDATE NOWAIT`, start, end)
	if err != nil {
		return safeError(err)
	}
	for rows.Next() {
		var at int64
		if err = rows.Scan(&at); err != nil {
			rows.Close()
			return safeError(err)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return safeError(err)
	}
	keys := make([]string, 0, len(nodes))
	for node := range nodes {
		keys = append(keys, node)
	}
	sort.Strings(keys)
	for _, node := range keys {
		var id []byte
		if err = conn.QueryRowContext(ctx, `SELECT id FROM nodes WHERE id=? FOR UPDATE NOWAIT`, []byte(node)).Scan(&id); err != nil {
			return safeError(err)
		}
	}
	keys = keys[:0]
	for batch := range batches {
		keys = append(keys, batch)
	}
	sort.Strings(keys)
	for _, batch := range keys {
		var id []byte
		if err = conn.QueryRowContext(ctx, `SELECT batch_id FROM telemetry_ingest_batches WHERE batch_id=? FOR UPDATE NOWAIT`, []byte(batch)).Scan(&id); err != nil {
			return safeError(err)
		}
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO `+name+`(node_id,batch_id,sampled_at,metric,value) SELECT source.node_id,source.batch_id,source.sampled_at,source.metric,source.value FROM telemetry_samples AS source WHERE source.sampled_at>=? AND source.sampled_at<? AND NOT EXISTS(SELECT 1 FROM `+name+` AS target WHERE source.sampled_at=target.sampled_at AND source.node_id=target.node_id AND source.batch_id=target.batch_id AND BINARY source.metric=BINARY target.metric)`, start, end)
	if err != nil {
		return safeError(err)
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
	n, err := deleted.RowsAffected()
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
	rows, err := conn.QueryContext(ctx, `SELECT table_name,start_at,end_at FROM telemetry_sample_shards WHERE state='retired' ORDER BY start_at LIMIT 1`)
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
