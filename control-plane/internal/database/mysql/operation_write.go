package mysql

import (
	"context"
	"fmt"

	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) InsertIntent(ctx context.Context, v operationstore.QueuedIntent) error {
	_, err := s.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,version,request_id,trace_id,idempotency_key,request_hash,expires_at,created_at,updated_at)
		VALUES(?,?,?,?,'queued',1,?,?,?,?,?,?,?)`, UUIDBytes(v.ID), UUIDBytes(v.WorkspaceID), UUIDBytes(v.NodeID), UUIDBytes(v.CommandID), v.RequestID, v.TraceID, v.IdempotencyKey, v.RequestHash, v.ExpiresAt, v.CreatedAt, v.CreatedAt)
	return err
}

func (s operationStore) EnqueueCommand(ctx context.Context, v operationstore.QueuedCommand) error {
	if _, err := s.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at,resource_type,resource_key)
		VALUES(?,?,?,?,'queued',?,?,?,?,?,?,?,?,?,?)`, UUIDBytes(v.ID), UUIDBytes(v.OperationID), UUIDBytes(v.WorkspaceID), UUIDBytes(v.NodeID), v.PayloadType, v.Envelope, v.IdempotencyKey, v.ExpectedVersion, v.Traceparent, v.ExpiresAt, v.CreatedAt, v.CreatedAt, v.ResourceType, v.ResourceKey); err != nil {
		return fmt.Errorf("insert command: %w", err)
	}
	if _, err := s.Exec(ctx, `INSERT INTO outbox_events(id,command_id,event_type,payload,available_at,created_at)
		VALUES(?,?,'command.dispatch',?,?,?)`, UUIDBytes(v.OutboxID), UUIDBytes(v.ID), v.Envelope, v.AvailableAt, v.CreatedAt); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	_, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'queued',?)`, UUIDBytes(v.EventID), UUIDBytes(v.OperationID), v.CreatedAt)
	return err
}

func (s operationStore) NotifyOutbox(context.Context, uuid.UUID) error {
	// Durable polling, not a notification, is the dispatch authority.
	return nil
}
