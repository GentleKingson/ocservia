package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type auditStore struct{ database.Store }

func (b *Backend) AuditStore() auditstore.Store     { return auditStore{b} }
func (t *transaction) AuditStore() auditstore.Store { return auditStore{t} }
func (s auditStore) Lock(ctx context.Context, workspace uuid.UUID) error {
	tx, ok := s.Store.(database.Tx)
	if !ok {
		return database.ErrUnsupported
	}
	return LockTransaction(ctx, tx, "audit-chain:"+workspace.String())
}
func (s auditStore) Clock(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := s.QueryRow(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&at)
	return at, err
}
func (s auditStore) Previous(ctx context.Context, workspace uuid.UUID) ([]byte, error) {
	var hash []byte
	err := s.QueryRow(ctx, `SELECT event_hash FROM audit_events WHERE workspace_id=? ORDER BY occurred_at DESC,id DESC LIMIT 1 FOR UPDATE`, UUIDBytes(workspace)).Scan(&hash)
	return hash, err
}
func (s auditStore) Append(ctx context.Context, args []any) error {
	args = append([]any(nil), args...)
	for i, arg := range args {
		switch v := arg.(type) {
		case uuid.UUID:
			args[i] = UUIDBytes(v)
		case *uuid.UUID:
			args[i] = nil
			if v != nil {
				args[i] = UUIDBytes(*v)
			}
		}
	}
	_, err := s.Exec(ctx, `INSERT INTO audit_events (id,workspace_id,occurred_at,actor_type,actor_id,source_session_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,approval_id,result,reason,before_summary,after_summary,error_type,previous_event_hash,event_hash,auth_version,event_key_id,event_mac) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
	return err
}
func (s auditStore) Events(ctx context.Context, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,COALESCE(trace_id,''),result,COALESCE(reason,''),source_session_id,node_id,command_id,approval_id,before_summary,after_summary,COALESCE(error_type,''),previous_event_hash,event_hash,auth_version,event_key_id,event_mac FROM audit_events WHERE workspace_id=? ORDER BY occurred_at,id`, UUIDBytes(workspace))
}
func (s auditStore) Checkpoints(ctx context.Context, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT through_event_id,through_event_hash,signature FROM audit_checkpoints WHERE workspace_id=? ORDER BY created_at,id`, UUIDBytes(workspace))
}
func (s auditStore) AddCheckpoint(ctx context.Context, id, workspace, event uuid.UUID, hash, signature []byte) error {
	// The workspace chain lock serializes this insert without runtime UPDATE rights.
	var existing []byte
	err := s.QueryRow(ctx, `SELECT through_event_hash FROM audit_checkpoints WHERE workspace_id=? AND through_event_id=?`, UUIDBytes(workspace), UUIDBytes(event)).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, database.ErrNotFound) {
		return err
	}
	tx, ok := s.Store.(database.Tx)
	if !ok {
		return database.ErrUnsupported
	}
	now, err := database.TransactionTime(ctx, tx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO audit_checkpoints(id,workspace_id,through_event_id,through_event_hash,signature,created_at) VALUES(?,?,?,?,?,?)`, UUIDBytes(id), UUIDBytes(workspace), UUIDBytes(event), hash, signature, now)
	return err
}
func (s auditStore) Workspaces(ctx context.Context) (database.Rows, error) {
	return s.Query(ctx, `SELECT DISTINCT workspace_id FROM audit_events ORDER BY workspace_id`)
}
func (s auditStore) CheckpointHash(ctx context.Context, workspace, event uuid.UUID) ([]byte, error) {
	var hash []byte
	err := s.QueryRow(ctx, `SELECT through_event_hash FROM audit_checkpoints WHERE workspace_id=? AND through_event_id=?`, UUIDBytes(workspace), UUIDBytes(event)).Scan(&hash)
	return hash, err
}
