package mysql

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
	"github.com/google/uuid"
)

// UsageStore borrows the telemetry/user-operation transaction. As on
// PostgreSQL, node locking serializes missing cursors too. This store never
// commits or opens a second transaction.
type UsageStore struct{ tx database.Tx }

func NewUsageStore(tx database.Tx) *UsageStore { return &UsageStore{tx: tx} }

func (t *transaction) UsageStore() userusage.Store { return NewUsageStore(t) }

func (s *UsageStore) LockNode(ctx context.Context, nodeID uuid.UUID) error {
	var id []byte
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=? FOR UPDATE`, UUIDBytes(nodeID)).Scan(&id)
}

func (s *UsageStore) LockCursor(ctx context.Context, nodeID uuid.UUID, sample userusage.Cursor) (userusage.Cursor, error) {
	prior := userusage.Cursor{SessionID: sample.SessionID, Connected: sample.Connected}
	err := s.tx.QueryRow(ctx, `SELECT username,rx_bytes,tx_bytes,observed_at FROM user_usage_cursors WHERE node_id=? AND session_id=? AND connected_at=? FOR UPDATE`, UUIDBytes(nodeID), sample.SessionID, sample.Connected).Scan(&prior.Username, &prior.RXBytes, &prior.TXBytes, &prior.ObservedAt)
	return prior, err
}

func (s *UsageStore) PutCursor(ctx context.Context, nodeID uuid.UUID, sample userusage.Cursor) error {
	// MySQL evaluates assignments left-to-right. Keep observed_at last so
	// every conditional compares against the same stored observation time.
	_, err := s.tx.Exec(ctx, `INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE
		username=IF(VALUES(observed_at)>observed_at,VALUES(username),username),
		rx_bytes=IF(VALUES(observed_at)>observed_at,VALUES(rx_bytes),rx_bytes),
		tx_bytes=IF(VALUES(observed_at)>observed_at,VALUES(tx_bytes),tx_bytes),
		observed_at=GREATEST(observed_at,VALUES(observed_at))`, UUIDBytes(nodeID), sample.SessionID, sample.Connected, sample.Username, sample.RXBytes, sample.TXBytes, sample.ObservedAt)
	return err
}

func (s *UsageStore) AddUsage(ctx context.Context, nodeID uuid.UUID, sample userusage.Cursor, period string, start value.Timestamp, rx, tx int64) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE
		rx_bytes=LEAST(9223372036854775807,CAST(rx_bytes AS DECIMAL(20,0))+CAST(VALUES(rx_bytes) AS DECIMAL(20,0))),
		tx_bytes=LEAST(9223372036854775807,CAST(tx_bytes AS DECIMAL(20,0))+CAST(VALUES(tx_bytes) AS DECIMAL(20,0))),
		observed_at=GREATEST(observed_at,VALUES(observed_at))`, UUIDBytes(nodeID), sample.Username, period, start, rx, tx, sample.ObservedAt)
	return err
}

var _ userusage.Store = (*UsageStore)(nil)
