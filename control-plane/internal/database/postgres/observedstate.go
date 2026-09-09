package postgres

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/observedstate"
	"github.com/google/uuid"
)

type ObservedStateStore struct{ tx database.Tx }

func (t *transaction) ObservedStateStore() observedstate.Store { return &ObservedStateStore{tx: t} }
func (s *ObservedStateStore) LockNode(ctx context.Context, node uuid.UUID) error {
	var id uuid.UUID
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, node).Scan(&id)
}
func (s *ObservedStateStore) ReplaceGroups(ctx context.Context, node uuid.UUID, groups []observedstate.Group) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM observed_groups WHERE node_id=$1`, node); err != nil {
		return err
	}
	for _, g := range groups {
		if _, err := s.tx.Exec(ctx, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES($1,$2,$3,$4,$5,$6)`, node, g.Name, g.Members, g.Revision, g.Fingerprint, g.ObservedAt); err != nil {
			return err
		}
	}
	return nil
}
func (s *ObservedStateStore) Groups(ctx context.Context, node uuid.UUID) ([]observedstate.Group, error) {
	rows, err := s.tx.Query(ctx, `SELECT group_name,members,revision,fingerprint,observed_at FROM observed_groups WHERE node_id=$1 ORDER BY group_name COLLATE "C"`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []observedstate.Group{}
	for rows.Next() {
		var g observedstate.Group
		if err := rows.Scan(&g.Name, &g.Members, &g.Revision, &g.Fingerprint, &g.ObservedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}
func (s *ObservedStateStore) InsertSecurity(ctx context.Context, node uuid.UUID, e observedstate.SecurityEvent) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, e.ID, node, e.ObservedAt, e.Severity, e.Type, e.Detail)
	return err
}
func (s *ObservedStateStore) SecurityEvent(ctx context.Context, id uuid.UUID) (observedstate.SecurityEvent, error) {
	e := observedstate.SecurityEvent{ID: id}
	err := s.tx.QueryRow(ctx, `SELECT observed_at,severity,event_type,detail FROM telemetry_security_events WHERE event_id=$1`, id).Scan(&e.ObservedAt, &e.Severity, &e.Type, &e.Detail)
	return e, err
}
