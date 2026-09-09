package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

var ErrTelemetryHistoryPending = errors.New("experimental database: owner telemetry-migrate-history is required")

const telemetryRetireShardsV4DDL = `CREATE PROCEDURE telemetry_retire_shards(IN cutoff BIGINT)
SQL SECURITY DEFINER
BEGIN
 DECLARE clock_at BIGINT;
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 SET clock_at = TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6));
 IF cutoff IS NULL OR cutoff < clock_at-7776000000000 OR cutoff > clock_at THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry retention cutoff outside permitted window';
 END IF;
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 IF guard_key IS NULL THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry catalog guard is unavailable';
 END IF;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF migration_state IS NULL OR migration_state<>'complete' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry history migration is incomplete';
 END IF;
 UPDATE telemetry_sample_shards SET state='retired' WHERE state='active' AND end_at<=cutoff;
END`

// TelemetryLegacyMigrationSteps is independent v4 authoring input. The v1-v3
// manifests and their SQL builders are not rewritten by this data migration.
func TelemetryLegacyMigrationSteps() []LongKeyStep {
	steps := []LongKeyStep{
		{Name: "telemetry_legacy_migration", Kind: "table", Object: "telemetry_legacy_migration", SQL: `CREATE TABLE telemetry_legacy_migration (
 singleton TINYINT NOT NULL PRIMARY KEY CHECK(singleton=1),
 state VARBINARY(16) NOT NULL CHECK(state IN ('pending','running','complete')),
 completed_at DATETIME(6) NULL,
 CHECK((state='complete')=(completed_at IS NOT NULL))
) ENGINE=InnoDB`},
		{Name: "telemetry_legacy_pending", Kind: "data", Object: "telemetry_legacy_migration", SQL: `INSERT INTO telemetry_legacy_migration(singleton,state,completed_at) SELECT 1,'pending',NULL WHERE NOT EXISTS(SELECT 1 FROM telemetry_legacy_migration WHERE singleton=1)`, VerifySQL: `SELECT IF(COUNT(*)=1,'valid','invalid') FROM telemetry_legacy_migration WHERE singleton=1`, Repairable: true},
	}
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := "telemetry_legacy_insert_guard"
		if event == "UPDATE" {
			name = "telemetry_legacy_update_guard"
		}
		body := fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON telemetry_samples FOR EACH ROW
BEGIN
 DECLARE guard_key VARBINARY(128);
 DECLARE migration_state VARBINARY(16);
 SELECT lock_key INTO guard_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE;
 SELECT state INTO migration_state FROM telemetry_legacy_migration WHERE singleton=1 LOCK IN SHARE MODE;
 IF guard_key IS NULL OR migration_state IS NULL THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='telemetry legacy migration metadata is unavailable';
 END IF;
 IF migration_state='complete' AND NEW.sampled_at NOT IN (-9223372036854775808,9223372036854775807) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='finite telemetry must use its monthly shard';
 END IF;
END`, name, event)
		steps = append(steps, LongKeyStep{Name: name, Kind: "trigger", Object: name, SQL: body})
	}
	steps = append(steps,
		LongKeyStep{Name: "telemetry_retire_before_v4", Kind: "procedure", Object: "telemetry_retire_shards", SQL: `DROP PROCEDURE telemetry_retire_shards`},
		LongKeyStep{Name: "telemetry_retire_ready_v4", Kind: "procedure", Object: "telemetry_retire_shards", SQL: telemetryRetireShardsV4DDL},
	)
	return steps
}

// MigrateTelemetryHistory discovers every remaining finite month, not only
// recent ingestion months. Existing planned receipts resume first. Complete
// months remain committed after interruption; each individual move is atomic.
// Infinity values stay in the default table, as in PostgreSQL's default
// partition: they do not belong to any finite calendar month.
func (b *Backend) MigrateTelemetryHistory(ctx context.Context) (result error) {
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, lock)) }()
	// Check owner authority without changing any receipt, including when a
	// checksum or schema conflict will subsequently refuse the operation.
	if _, err = conn.ExecContext(ctx, `UPDATE telemetry_legacy_migration SET singleton=singleton WHERE singleton=0`); err != nil {
		return safeError(err)
	}
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		return err
	}
	if _, err = b.verifiedSnapshotOn(ctx, conn, chain); err != nil {
		return err
	}
	// Even a completed repeat requires owner metadata-write privileges.
	changed, err := conn.ExecContext(ctx, `UPDATE telemetry_legacy_migration SET state=IF(state='pending','running',state) WHERE singleton=1`)
	if err != nil {
		return safeError(err)
	}
	if _, err = changed.RowsAffected(); err != nil {
		return safeError(err)
	}
	var state string
	if err = conn.QueryRowContext(ctx, `SELECT state FROM telemetry_legacy_migration WHERE singleton=1`).Scan(&state); err != nil {
		return safeError(err)
	}
	if state != "running" && state != "complete" {
		return ErrSchema
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		var planned string
		err = conn.QueryRowContext(ctx, `SELECT table_name FROM telemetry_sample_shards WHERE state='planned' ORDER BY start_at LIMIT 1`).Scan(&planned)
		if err == nil {
			month, err := telemetryMonthFromName(planned)
			if err != nil {
				return err
			}
			if err = b.provisionTelemetryMonth(ctx, conn, month); err != nil {
				return err
			}
			continue
		}
		if err != sql.ErrNoRows {
			return safeError(err)
		}
		var earliest sql.NullInt64
		if err = conn.QueryRowContext(ctx, `SELECT MIN(sampled_at) FROM telemetry_samples WHERE sampled_at NOT IN (-9223372036854775808,9223372036854775807)`).Scan(&earliest); err != nil {
			return safeError(err)
		}
		if earliest.Valid {
			if state == "complete" {
				return ErrSchema
			}
			month, err := (value.Timestamp{Micros: earliest.Int64, Valid: true}).Time()
			if err != nil {
				return err
			}
			if err = b.provisionTelemetryMonth(ctx, conn, month); err != nil {
				return err
			}
			continue
		}
		snapshot, err := b.verifiedSnapshotOn(ctx, conn, chain)
		if err != nil {
			return err
		}
		if err = validateRevisionSnapshot(ctx, conn, snapshot); err != nil {
			return err
		}
		complete, err := finishTelemetryHistory(ctx, conn)
		if err != nil {
			return err
		}
		if complete {
			return validateRevisionSnapshot(ctx, conn, snapshot)
		}
	}
}

func finishTelemetryHistory(ctx context.Context, conn *sql.Conn) (complete bool, result error) {
	if _, err := conn.ExecContext(ctx, `SET TRANSACTION ISOLATION LEVEL SERIALIZABLE`); err != nil {
		return false, safeError(err)
	}
	if _, err := conn.ExecContext(ctx, `START TRANSACTION`); err != nil {
		return false, safeError(err)
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
	var key []byte
	if err := conn.QueryRowContext(ctx, `SELECT lock_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' FOR UPDATE`).Scan(&key); err != nil {
		return false, safeError(err)
	}
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM telemetry_legacy_migration WHERE singleton=1 FOR UPDATE`).Scan(&state); err != nil {
		return false, safeError(err)
	}
	if state != "running" && state != "complete" {
		return false, ErrSchema
	}
	var finite int64
	err := conn.QueryRowContext(ctx, `SELECT sampled_at FROM telemetry_samples WHERE sampled_at NOT IN (-9223372036854775808,9223372036854775807) LIMIT 1 FOR UPDATE NOWAIT`).Scan(&finite)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, safeError(err)
	}
	if _, err = conn.ExecContext(ctx, `UPDATE telemetry_legacy_migration SET state='complete',completed_at=CURRENT_TIMESTAMP(6) WHERE singleton=1 AND state='running'`); err != nil {
		return false, safeError(err)
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, safeError(err)
	}
	active = false
	return true, nil
}

// ValidateTelemetryHistoryReady is a read-only business-startup gate, separate
// from schema version validation. A structurally valid v4 database may still
// have an interrupted historical-data migration.
func (b *Backend) ValidateTelemetryHistoryReady(ctx context.Context) error {
	var state string
	if err := b.QueryRow(ctx, `SELECT state FROM telemetry_legacy_migration WHERE singleton=1`).Scan(&state); err != nil {
		return err
	}
	if state != "complete" {
		return ErrTelemetryHistoryPending
	}
	var finite bool
	if err := b.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telemetry_samples WHERE sampled_at NOT IN (-9223372036854775808,9223372036854775807))`).Scan(&finite); err != nil {
		return err
	}
	if finite {
		return ErrSchema
	}
	return nil
}
