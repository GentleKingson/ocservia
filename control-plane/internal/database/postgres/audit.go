package postgres

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type auditStore struct{ database.Store }

func (b *Backend) AuditStore() auditstore.Store     { return auditStore{b} }
func (t *transaction) AuditStore() auditstore.Store { return auditStore{t} }
func (s auditStore) Lock(ctx context.Context, workspace uuid.UUID) error {
	_, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, workspace.String())
	return err
}
func (s auditStore) Clock(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := s.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return at, err
}
func (s auditStore) Previous(ctx context.Context, workspace uuid.UUID) ([]byte, error) {
	var hash []byte
	err := s.QueryRow(ctx, `SELECT event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspace).Scan(&hash)
	return hash, err
}
func (s auditStore) Append(ctx context.Context, args []any) error {
	_, err := s.Exec(ctx, `INSERT INTO audit_events (id,workspace_id,occurred_at,actor_type,actor_id,source_session_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,approval_id,result,reason,before_summary,after_summary,error_type,previous_event_hash,event_hash,auth_version,event_key_id,event_mac) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, args...)
	return err
}
func (s auditStore) Events(ctx context.Context, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,COALESCE(trace_id,''),result,COALESCE(reason,''),source_session_id,node_id,command_id,approval_id,before_summary,after_summary,COALESCE(error_type,''),previous_event_hash,event_hash,auth_version,event_key_id,event_mac FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at,id`, workspace)
}
func (s auditStore) Checkpoints(ctx context.Context, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT through_event_id,through_event_hash,signature FROM audit_checkpoints WHERE workspace_id=$1 ORDER BY created_at,id`, workspace)
}
func (s auditStore) AddCheckpoint(ctx context.Context, id, workspace, event uuid.UUID, hash, signature []byte) error {
	_, err := s.Exec(ctx, `INSERT INTO audit_checkpoints(id,workspace_id,through_event_id,through_event_hash,signature,created_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(workspace_id,through_event_id) DO NOTHING`, id, workspace, event, hash, signature)
	return err
}
func (s auditStore) Workspaces(ctx context.Context) (database.Rows, error) {
	return s.Query(ctx, `SELECT DISTINCT workspace_id FROM audit_events ORDER BY workspace_id`)
}
func (s auditStore) CheckpointHash(ctx context.Context, workspace, event uuid.UUID) ([]byte, error) {
	var hash []byte
	err := s.QueryRow(ctx, `SELECT through_event_hash FROM audit_checkpoints WHERE workspace_id=$1 AND through_event_id=$2`, workspace, event).Scan(&hash)
	return hash, err
}
