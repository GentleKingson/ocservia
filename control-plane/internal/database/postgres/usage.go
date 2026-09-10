package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
	"github.com/google/uuid"
)

// UsageStore borrows a transaction; it cannot open or commit another one.
type UsageStore struct{ tx database.Tx }

func NewUsageStore(tx database.Tx) *UsageStore { return &UsageStore{tx: tx} }

func (t *transaction) UsageStore() userusage.Store { return NewUsageStore(t) }

func (s *UsageStore) LockNode(ctx context.Context, nodeID uuid.UUID) error {
	var id uuid.UUID
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&id)
}

func (s *UsageStore) LockCursor(ctx context.Context, nodeID uuid.UUID, sample userusage.Cursor) (userusage.Cursor, error) {
	prior := userusage.Cursor{SessionID: sample.SessionID, Connected: sample.Connected}
	err := s.tx.QueryRow(ctx, `SELECT username,rx_bytes,tx_bytes,observed_at FROM user_usage_cursors WHERE node_id=$1 AND session_id=$2 AND connected_at=$3 FOR UPDATE`, nodeID, sample.SessionID, sample.Connected).Scan(&prior.Username, &prior.RXBytes, &prior.TXBytes, &prior.ObservedAt)
	return prior, err
}

func (s *UsageStore) PutCursor(ctx context.Context, nodeID uuid.UUID, sample userusage.Cursor) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(node_id,session_id,connected_at) DO UPDATE SET username=EXCLUDED.username,rx_bytes=EXCLUDED.rx_bytes,tx_bytes=EXCLUDED.tx_bytes,observed_at=EXCLUDED.observed_at WHERE EXCLUDED.observed_at>user_usage_cursors.observed_at`, nodeID, sample.SessionID, sample.Connected, sample.Username, sample.RXBytes, sample.TXBytes, sample.ObservedAt)
	return err
}

func (s *UsageStore) AddUsage(ctx context.Context, nodeID uuid.UUID, sample userusage.Cursor, period string, start value.Timestamp, rx, tx int64) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(node_id,username,period,period_start) DO UPDATE SET rx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.rx_bytes::numeric+EXCLUDED.rx_bytes::numeric)::bigint,tx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.tx_bytes::numeric+EXCLUDED.tx_bytes::numeric)::bigint,observed_at=GREATEST(observed_user_usage.observed_at,EXCLUDED.observed_at)`, nodeID, sample.Username, period, start, rx, tx, sample.ObservedAt)
	return err
}

var _ userusage.Store = (*UsageStore)(nil)
