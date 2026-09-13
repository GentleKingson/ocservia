package mysql

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryhistory"
	"github.com/google/uuid"
)

// Each month is an ordinary InnoDB table, not a native MySQL partition, so
// both node RESTRICT and ingest-batch CASCADE foreign keys remain enforceable.
// Runtime cannot create tables. The owner provisions months before ingestion.
type TelemetryHistoryStore struct{ tx database.Tx }

func NewTelemetryHistoryStore(tx database.Tx) *TelemetryHistoryStore {
	return &TelemetryHistoryStore{tx: tx}
}
func (t *transaction) TelemetryHistoryStore() telemetryhistory.Store {
	return NewTelemetryHistoryStore(t)
}

var telemetryShardName = regexp.MustCompile(`\Atelemetry_samples_(m_[0-9]{6}|x_[pn][0-9]{7})\z`)

func (s *TelemetryHistoryStore) lockCatalog(ctx context.Context) error {
	var key []byte
	return s.tx.QueryRow(ctx, `SELECT lock_key FROM business_locks WHERE lock_key='telemetry-shard-catalog' LOCK IN SHARE MODE`).Scan(&key)
}

func telemetryMicros(t time.Time) (int64, error) {
	v, err := value.FromTime(t)
	return v.Micros, err
}

// Locking catalog rows keeps every referenced table alive until this Tx ends.
// The catalog is append-only except for active -> retired -> dropped states.
func (s *TelemetryHistoryStore) activeTables(ctx context.Context, since int64) ([]string, error) {
	if err := s.lockCatalog(ctx); err != nil {
		return nil, err
	}
	rows, err := s.tx.Query(ctx, `SELECT table_name FROM telemetry_sample_shards WHERE state='active' AND end_at>? ORDER BY start_at LOCK IN SHARE MODE`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := []string{"telemetry_samples"}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if !telemetryShardName.MatchString(name) {
			return nil, ErrSchema
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (s *TelemetryHistoryStore) Insert(ctx context.Context, nodeID, batchID uuid.UUID, samples []telemetryhistory.Sample) error {
	if err := s.lockCatalog(ctx); err != nil {
		return err
	}
	for _, sample := range samples {
		at, err := telemetryMicros(sample.SampledAt)
		if err != nil {
			return err
		}
		var name string
		if err := s.tx.QueryRow(ctx, `SELECT table_name FROM telemetry_sample_shards WHERE start_at<=? AND end_at>? AND state='active' LOCK IN SHARE MODE`, at, at).Scan(&name); err != nil {
			return fmt.Errorf("telemetry month is not provisioned: %w", err)
		}
		if !telemetryShardName.MatchString(name) {
			return ErrSchema
		}
		// Duplicate handling is confined to the same sample PK. IGNORE would
		// also suppress foreign-key and CHECK violations, so it is forbidden.
		_, err = s.tx.Exec(ctx, `INSERT INTO `+name+`(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,?,?)`, UUIDBytes(nodeID), UUIDBytes(batchID), at, sample.Metric, sample.Value)
		if err != nil && !errors.Is(err, database.ErrUnique) {
			return fmt.Errorf("insert telemetry sample: %w", err)
		}
	}
	return nil
}

func (s *TelemetryHistoryStore) History(ctx context.Context, nodeID uuid.UUID, metric, resolution string, since value.Timestamp) ([]telemetryhistory.Point, error) {
	if err := since.Validate(); err != nil {
		return nil, err
	}
	if !since.Valid {
		return nil, database.ErrConstraint
	}
	at := since.Micros
	var query string
	var args []any
	switch resolution {
	case "raw":
		tables, err := s.activeTables(ctx, at)
		if err != nil {
			return nil, err
		}
		parts := make([]string, 0, len(tables))
		for _, name := range tables {
			parts = append(parts, `SELECT sampled_at AS at,metric,1 AS sample_count,value AS min_value,value AS max_value,value AS avg_value FROM `+name+` WHERE node_id=? AND BINARY metric=BINARY ? AND sampled_at>=?`)
			args = append(args, UUIDBytes(nodeID), metric, at)
		}
		query = `SELECT * FROM (` + strings.Join(parts, ` UNION ALL `) + `) AS samples ORDER BY at LIMIT 2000`
	case "5m", "1h":
		query = `SELECT bucket_at,metric,sample_count,min_value,max_value,avg_value FROM telemetry_rollups_` + resolution + ` WHERE node_id=? AND BINARY metric=BINARY ? AND bucket_at>=? ORDER BY bucket_at LIMIT 2000`
		args = []any{UUIDBytes(nodeID), metric, at}
	default:
		return nil, database.ErrConstraint
	}
	rows, err := s.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []telemetryhistory.Point{}
	for rows.Next() {
		var p telemetryhistory.Point
		if err := rows.Scan(&p.At, &p.Metric, &p.Count, &p.Minimum, &p.Maximum, &p.Average); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

func (s *TelemetryHistoryStore) Maintain(ctx context.Context, now time.Time) error {
	now = now.UTC()
	since, err := telemetryMicros(now.Add(-14 * 24 * time.Hour).Truncate(time.Hour))
	if err != nil {
		return err
	}
	tables, err := s.activeTables(ctx, since)
	if err != nil {
		return err
	}
	if err := s.rollup(ctx, tables, now.Add(-14*24*time.Hour)); err != nil {
		return fmt.Errorf("roll up recent telemetry: %w", err)
	}
	cutRaw, err := telemetryMicros(now.Add(-14 * 24 * time.Hour))
	if err != nil {
		return err
	}
	// Finalize the one retirement candidate even after a long maintenance
	// outage. Its catalog lock holds through rollups, retirement and fencing.
	var candidate string
	var start value.Timestamp
	err = s.tx.QueryRow(ctx, `SELECT table_name,start_at FROM telemetry_sample_shards WHERE state='active' AND end_at<=LEAST(?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))-1209600000000) ORDER BY start_at LIMIT 1 LOCK IN SHARE MODE`, cutRaw).Scan(&candidate, &start)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return err
	}
	if err == nil {
		if !telemetryShardName.MatchString(candidate) {
			return ErrSchema
		}
		begin, err := start.Time()
		if err != nil {
			return err
		}
		if err := s.rollup(ctx, []string{candidate}, begin); err != nil {
			return err
		}
	}
	at, err := telemetryMicros(now)
	if err != nil {
		return err
	}
	if _, err = s.tx.Exec(ctx, `CALL telemetry_prune_rollups(?)`, at); err != nil {
		return fmt.Errorf("prune telemetry rollups: %w", err)
	}
	_, err = s.tx.Exec(ctx, `CALL telemetry_retire_shards(?)`, cutRaw)
	if err != nil {
		return fmt.Errorf("retire telemetry month: %w", err)
	}
	return nil
}

func (s *TelemetryHistoryStore) rollup(ctx context.Context, tables []string, begin time.Time) error {
	for _, rollup := range []struct {
		suffix string
		width  int64
	}{{"5m", 300000000}, {"1h", 3600000000}} {
		// Cover accepted late samples without replacing the first bucket with
		// a partial aggregate. The table list covers the widest (hour) bucket.
		since, err := telemetryMicros(begin.Truncate(time.Duration(rollup.width) * time.Microsecond))
		if err != nil {
			return err
		}
		name := "telemetry_rollups_" + rollup.suffix
		if err := LockExactKey(ctx, s.tx, name); err != nil {
			return err
		}
		parts := make([]string, 0, len(tables))
		args := make([]any, 0, len(tables))
		for _, table := range tables {
			parts = append(parts, `SELECT node_id,metric,sampled_at,value FROM `+table+` WHERE sampled_at>=?`)
			args = append(args, since)
		}
		// PostgreSQL date_bin preserves infinities. They are ordered values,
		// not finite microseconds to round (which would corrupt the sentinel).
		query := fmt.Sprintf(`SELECT node_id,metric,CASE WHEN sampled_at IN (%d,%d) THEN sampled_at ELSE FLOOR(CAST(sampled_at AS DECIMAL(20,0))/%d)*%d END AS bucket_at,COUNT(*),MIN(value),MAX(value),AVG(value) FROM (%s) AS samples GROUP BY node_id,metric,bucket_at`, value.NegativeInfinity, value.PositiveInfinity, rollup.width, rollup.width, strings.Join(parts, ` UNION ALL `))
		rows, err := s.tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		type aggregate struct {
			node          []byte
			metric        string
			bucket, count int64
			min, max, avg float64
		}
		var aggregates []aggregate
		for rows.Next() {
			var a aggregate
			if err := rows.Scan(&a.node, &a.metric, &a.bucket, &a.count, &a.min, &a.max, &a.avg); err != nil {
				rows.Close()
				return err
			}
			aggregates = append(aggregates, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, a := range aggregates {
			var id uint64
			err = s.tx.QueryRow(ctx, `SELECT exact_row_id FROM `+name+` WHERE node_id=? AND BINARY metric=BINARY ? AND bucket_at=? FOR UPDATE`, a.node, a.metric, a.bucket).Scan(&id)
			if errors.Is(err, database.ErrNotFound) {
				_, err = s.tx.Exec(ctx, `INSERT INTO `+name+`(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES(?,?,?,?,?,?,?)`, a.node, a.metric, a.bucket, a.count, a.min, a.max, a.avg)
			} else if err == nil {
				_, err = s.tx.Exec(ctx, `UPDATE `+name+` SET sample_count=?,min_value=?,max_value=?,avg_value=? WHERE exact_row_id=?`, a.count, a.min, a.max, a.avg, id)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

var _ telemetryhistory.Store = (*TelemetryHistoryStore)(nil)
