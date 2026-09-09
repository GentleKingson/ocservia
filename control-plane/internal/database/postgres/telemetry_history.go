package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryhistory"
	"github.com/google/uuid"
)

type TelemetryHistoryStore struct{ tx database.Tx }

func NewTelemetryHistoryStore(tx database.Tx) *TelemetryHistoryStore {
	return &TelemetryHistoryStore{tx: tx}
}
func (t *transaction) TelemetryHistoryStore() telemetryhistory.Store {
	return NewTelemetryHistoryStore(t)
}

func (s *TelemetryHistoryStore) Insert(ctx context.Context, nodeID, batchID uuid.UUID, samples []telemetryhistory.Sample) error {
	for _, sample := range samples {
		if _, err := s.tx.Exec(ctx, `SELECT telemetry_ensure_month_partition($1)`, sample.SampledAt); err != nil {
			return fmt.Errorf("ensure telemetry partition: %w", err)
		}
		if _, err := s.tx.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, nodeID, batchID, sample.SampledAt, sample.Metric, sample.Value); err != nil {
			return fmt.Errorf("insert telemetry sample: %w", err)
		}
	}
	return nil
}

func (s *TelemetryHistoryStore) History(ctx context.Context, nodeID uuid.UUID, metric, resolution string, since time.Time) ([]telemetryhistory.Point, error) {
	query := `SELECT sampled_at,metric,1,value,value,value FROM telemetry_samples WHERE node_id=$1 AND metric=$2 AND sampled_at >= $3 ORDER BY sampled_at LIMIT 2000`
	switch resolution {
	case "raw":
	case "5m", "1h":
		query = fmt.Sprintf(`SELECT bucket_at,metric,sample_count,min_value,max_value,avg_value FROM telemetry_rollups_%s WHERE node_id=$1 AND metric=$2 AND bucket_at >= $3 ORDER BY bucket_at LIMIT 2000`, resolution)
	default:
		return nil, database.ErrConstraint
	}
	rows, err := s.tx.Query(ctx, query, nodeID, metric, since)
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
	for _, table := range []struct{ name, interval string }{{"telemetry_rollups_5m", "5 minutes"}, {"telemetry_rollups_1h", "1 hour"}} {
		_, err := s.tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT node_id,metric,date_bin($1::interval,sampled_at,'2000-01-01'::timestamptz),count(*),min(value),max(value),avg(value) FROM telemetry_samples WHERE sampled_at >= $2 GROUP BY 1,2,3 ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value`, table.name), table.interval, now.Add(-48*time.Hour))
		if err != nil {
			return err
		}
	}
	if _, err := s.tx.Exec(ctx, `DELETE FROM telemetry_rollups_5m WHERE bucket_at < ($1::timestamptz - interval '90 days')`, now); err != nil {
		return err
	}
	if _, err := s.tx.Exec(ctx, `DELETE FROM telemetry_rollups_1h WHERE bucket_at < ($1::timestamptz - interval '13 months')`, now); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `SELECT telemetry_drop_expired_partitions($1)`, now.Add(-14*24*time.Hour))
	return err
}

var _ telemetryhistory.Store = (*TelemetryHistoryStore)(nil)
