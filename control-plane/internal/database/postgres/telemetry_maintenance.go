package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
)

func (s telemetryWriteStore) LockOfflineCandidates(ctx context.Context, before time.Time) ([]uuid.UUID, error) {
	rows, err := s.tx.Query(ctx, `SELECT n.id FROM nodes n JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.status='active' AND o.last_heartbeat_at < $1 FOR UPDATE OF n SKIP LOCKED`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		nodes = append(nodes, id)
	}
	return nodes, rows.Err()
}

func (s telemetryWriteStore) MarkOffline(ctx context.Context, node, event uuid.UUID, now time.Time, traceparent string) error {
	if _, err := s.tx.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=$2,version=version+1 WHERE id=$1 AND status='active'`, node, now); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload) VALUES($1,$2,'disconnected',$3,$4,$5)`, event, node, now, traceparent, []byte("heartbeat timeout"))
	return err
}
