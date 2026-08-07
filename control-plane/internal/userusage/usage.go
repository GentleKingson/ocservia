// Package userusage converts node session counters into durable quota usage.
package userusage

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrInvalidSample = errors.New("usage sample is invalid")

type Sample struct {
	SessionID  string
	Username   string
	Connected  time.Time
	RXBytes    int64
	TXBytes    int64
	ObservedAt time.Time
}

// RecordTx applies monotonically increasing session samples exactly once.
func RecordTx(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, samples []Sample) error {
	for _, sample := range samples {
		if sample.RXBytes < 0 || sample.TXBytes < 0 || sample.ObservedAt.IsZero() || sample.Connected.IsZero() {
			return ErrInvalidSample
		}
		var priorRX, priorTX int64
		var priorUsername string
		var priorObservedAt time.Time
		err := tx.QueryRow(ctx, `SELECT username,rx_bytes,tx_bytes,observed_at FROM user_usage_cursors WHERE node_id=$1 AND session_id=$2 AND connected_at=$3 FOR UPDATE`, nodeID, sample.SessionID, sample.Connected).Scan(&priorUsername, &priorRX, &priorTX, &priorObservedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && !sample.ObservedAt.After(priorObservedAt) {
			continue
		}
		if err == nil && sample.Username != priorUsername {
			return ErrInvalidSample
		}
		deltaRX, deltaTX := sample.RXBytes-priorRX, sample.TXBytes-priorTX
		if deltaRX < 0 {
			deltaRX = sample.RXBytes
		}
		if deltaTX < 0 {
			deltaTX = sample.TXBytes
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(node_id,session_id,connected_at) DO UPDATE SET username=EXCLUDED.username,rx_bytes=EXCLUDED.rx_bytes,tx_bytes=EXCLUDED.tx_bytes,observed_at=EXCLUDED.observed_at WHERE EXCLUDED.observed_at>user_usage_cursors.observed_at`, nodeID, sample.SessionID, sample.Connected, sample.Username, sample.RXBytes, sample.TXBytes, sample.ObservedAt); err != nil {
			return err
		}
		for _, period := range []struct {
			kind  string
			start time.Time
		}{{"monthly", monthStart(sample.ObservedAt)}, {"lifetime", time.Unix(0, 0).UTC()}} {
			if _, err := tx.Exec(ctx, `INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(node_id,username,period,period_start) DO UPDATE SET rx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.rx_bytes::numeric+EXCLUDED.rx_bytes::numeric)::bigint,tx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.tx_bytes::numeric+EXCLUDED.tx_bytes::numeric)::bigint,observed_at=GREATEST(observed_user_usage.observed_at,EXCLUDED.observed_at)`, nodeID, sample.Username, period.kind, period.start, deltaRX, deltaTX, sample.ObservedAt); err != nil {
				return err
			}
		}
	}
	return nil
}

func monthStart(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
}
