package mysql

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func (s telemetryWriteStore) LockOfflineCandidates(ctx context.Context, before time.Time) ([]uuid.UUID, error) {
	at, err := value.FromTime(before)
	if err != nil {
		return nil, err
	}
	rows, err := s.tx.Query(ctx, `SELECT n.id FROM nodes n JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.status='active' AND o.last_heartbeat_at < ? FOR UPDATE SKIP LOCKED`, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []uuid.UUID
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := uuid.FromBytes(raw)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, id)
	}
	return nodes, rows.Err()
}

func (s telemetryWriteStore) MarkOffline(ctx context.Context, node, event uuid.UUID, now time.Time, traceparent string) error {
	at, err := value.FromTime(now)
	if err != nil {
		return err
	}
	if _, err := s.tx.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=?,version=version+1 WHERE id=? AND status='active'`, at, UUIDBytes(node)); err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload) VALUES(?,?,'disconnected',?,?,?)`, UUIDBytes(event), UUIDBytes(node), at, traceparent, []byte("heartbeat timeout"))
	return err
}
