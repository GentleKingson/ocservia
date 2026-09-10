package postgres

import (
	"context"
	"fmt"

	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) InsertIntent(ctx context.Context, v operationstore.QueuedIntent) error {
	_, err := s.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,version,request_id,trace_id,idempotency_key,request_hash,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,'queued',1,$5,$6,$7,$8,$9,$10,$10)`, v.ID, v.WorkspaceID, v.NodeID, v.CommandID, v.RequestID, v.TraceID, v.IdempotencyKey, v.RequestHash, v.ExpiresAt, v.CreatedAt)
	return err
}

func (s operationStore) EnqueueCommand(ctx context.Context, v operationstore.QueuedCommand) error {
	if _, err := s.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at,resource_type,resource_key)
		VALUES($1,$2,$3,$4,'queued',$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)`, v.ID, v.OperationID, v.WorkspaceID, v.NodeID, v.PayloadType, v.Envelope, v.IdempotencyKey, v.ExpectedVersion, v.Traceparent, v.ExpiresAt, v.CreatedAt, v.ResourceType, v.ResourceKey); err != nil {
		return fmt.Errorf("insert command: %w", err)
	}
	if _, err := s.Exec(ctx, `INSERT INTO outbox_events(id,command_id,event_type,payload,available_at,created_at)
		VALUES($1,$2,'command.dispatch',$3,$4,$5)`, v.OutboxID, v.ID, v.Envelope, v.AvailableAt, v.CreatedAt); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	_, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'queued',$3)`, v.EventID, v.OperationID, v.CreatedAt)
	return err
}

func (s operationStore) NotifyOutbox(ctx context.Context, id uuid.UUID) error {
	_, err := s.Exec(ctx, `SELECT pg_notify('ocservia_outbox',$1)`, id.String())
	return err
}
