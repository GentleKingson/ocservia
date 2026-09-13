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
	type shardBatch struct {
		name string
		args []any
	}
	var batches []shardBatch
	months := map[string]int{}
	for _, sample := range samples {
		at, err := telemetryMicros(sample.SampledAt)
		if err != nil {
			return err
		}
		month := sample.SampledAt.UTC().Format("2006-01")
		index, found := months[month]
		if !found {
			var name string
			if err := s.tx.QueryRow(ctx, `SELECT table_name FROM telemetry_sample_shards WHERE start_at<=? AND end_at>? AND state='active' LOCK IN SHARE MODE`, at, at).Scan(&name); err != nil {
				return fmt.Errorf("telemetry month is not provisioned: %w", err)
			}
			if !telemetryShardName.MatchString(name) {
				return ErrSchema
			}
			index = len(batches)
			months[month] = index
			batches = append(batches, shardBatch{name: name})
		}
		batches[index].args = append(batches[index].args, UUIDBytes(nodeID), UUIDBytes(batchID), at, sample.Metric, sample.Value)
	}
	for _, batch := range batches {
		for begin := 0; begin < len(batch.args); begin += 128 * 5 {
			end := min(begin+128*5, len(batch.args))
			args := batch.args[begin:end]
			prefix := `INSERT INTO ` + batch.name + `(node_id,batch_id,sampled_at,metric,value) VALUES`
			_, err := s.tx.Exec(ctx, prefix+strings.TrimSuffix(strings.Repeat("(?,?,?,?,?),", len(args)/5), ","), args...)
			if errors.Is(err, database.ErrUnique) {
				// InnoDB rolls back the entire failed INSERT. Replaying only
				// this chunk preserves first-sample-wins duplicate semantics;
				// IGNORE would incorrectly suppress FK and CHECK failures too.
				for i := 0; i < len(args); i += 5 {
					_, err = s.tx.Exec(ctx, prefix+"(?,?,?,?,?)", args[i:i+5]...)
					if err != nil && !errors.Is(err, database.ErrUnique) {
						return fmt.Errorf("insert telemetry sample: %w", err)
					}
				}
			} else if err != nil {
				return fmt.Errorf("insert telemetry samples: %w", err)
			}
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
	// Compatibility for callers explicitly borrowing one transaction. The
	// scheduler uses telemetryhistory.Maintain to commit between pages.
	for {
		done, err := s.MaintainBatch(ctx, now)
		if err != nil || done {
			return err
		}
	}
}

func (s *TelemetryHistoryStore) MaintainBatch(ctx context.Context, now time.Time) (bool, error) {
	cutRaw, err := telemetryMicros(now.Add(-14 * 24 * time.Hour))
	if err != nil {
		return false, err
	}
	var done bool
	if err := s.tx.QueryRow(ctx, `CALL telemetry_retire_shards(?)`, cutRaw).Scan(&done); err != nil {
		return false, fmt.Errorf("advance telemetry maintenance: %w", err)
	}
	if !done {
		return false, nil
	}
	at, err := telemetryMicros(now)
	if err != nil {
		return false, err
	}
	if _, err = s.tx.Exec(ctx, `CALL telemetry_prune_rollups(?)`, at); err != nil {
		return false, fmt.Errorf("prune telemetry rollups: %w", err)
	}
	return true, nil
}

func telemetryAggregateSQL(source string, width int64) string {
	return fmt.Sprintf(`SELECT node_id,metric,CASE WHEN sampled_at IN (%d,%d) THEN sampled_at ELSE FLOOR(CAST(sampled_at AS DECIMAL(20,0))/%d)*%d END AS bucket_at,COUNT(*) AS sample_count,MIN(value) AS min_value,MAX(value) AS max_value,AVG(value) AS avg_value FROM (%s) AS samples GROUP BY node_id,metric,bucket_at`, value.NegativeInfinity, value.PositiveInfinity, width, width, source)
}

func telemetryMergeSQL(table, aggregate string) []string {
	match := `r.node_id=a.node_id AND BINARY r.metric=BINARY a.metric AND r.bucket_at=a.bucket_at`
	return []string{
		`UPDATE ` + table + ` r JOIN (` + aggregate + `) a ON ` + match + ` SET r.sample_count=a.sample_count,r.min_value=a.min_value,r.max_value=a.max_value,r.avg_value=a.avg_value WHERE NOT (r.sample_count <=> a.sample_count AND r.min_value <=> a.min_value AND r.max_value <=> a.max_value AND r.avg_value <=> a.avg_value)`,
		`INSERT INTO ` + table + `(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT a.node_id,a.metric,a.bucket_at,a.sample_count,a.min_value,a.max_value,a.avg_value FROM (` + aggregate + `) a LEFT JOIN ` + table + ` r ON ` + match + ` WHERE r.exact_row_id IS NULL`,
	}
}

var _ telemetryhistory.Store = (*TelemetryHistoryStore)(nil)
