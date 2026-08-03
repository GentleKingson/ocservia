package audit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const chainResultIntent = "intent"

type ChainRecord struct {
	EventID                                  uuid.UUID
	WorkspaceID                              uuid.UUID
	ActorType, ActorID, Action, ResourceType string
	ResourceID                               uuid.UUID
	RequestID, TraceID, Reason               string
	At                                       time.Time
}

func LockChain(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, workspaceID.String()); err != nil {
		return fmt.Errorf("lock audit chain: %w", err)
	}
	return nil
}

func AppendChain(ctx context.Context, tx pgx.Tx, record ChainRecord) error {
	if err := LockChain(ctx, tx, record.WorkspaceID); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&record.At); err != nil {
		return fmt.Errorf("assign audit order: %w", err)
	}
	var previous []byte
	err := tx.QueryRow(ctx, `SELECT event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, record.WorkspaceID).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read audit chain: %w", err)
	}
	if record.EventID == uuid.Nil {
		record.EventID = uuid.Must(uuid.NewV7())
	}
	payload, err := encodeChainPayload(previous, record)
	if err != nil {
		return fmt.Errorf("encode audit payload: %w", err)
	}
	digest := sha256.Sum256(payload)
	var traceID any
	if record.TraceID != "" {
		traceID = record.TraceID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,trace_id,result,reason,previous_event_hash,event_hash) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, record.EventID, record.WorkspaceID, record.At, record.ActorType, record.ActorID, record.Action, record.ResourceType, record.ResourceID, record.RequestID, traceID, chainResultIntent, record.Reason, previous, digest[:])
	if err != nil {
		return fmt.Errorf("append audit intent: %w", err)
	}
	return nil
}

func encodeChainPayload(previous []byte, record ChainRecord) ([]byte, error) {
	return json.Marshal(struct {
		Previous     []byte    `json:"previous"`
		EventID      uuid.UUID `json:"event_id"`
		WorkspaceID  uuid.UUID `json:"workspace_id"`
		OccurredAt   time.Time `json:"occurred_at"`
		ActorType    string    `json:"actor_type"`
		ActorID      string    `json:"actor_id"`
		Action       string    `json:"action"`
		ResourceType string    `json:"resource_type"`
		ResourceID   uuid.UUID `json:"resource_id"`
		RequestID    string    `json:"request_id"`
		TraceID      string    `json:"trace_id"`
		Result       string    `json:"result"`
		Reason       string    `json:"reason"`
	}{
		Previous: previous, EventID: record.EventID, WorkspaceID: record.WorkspaceID, OccurredAt: record.At,
		ActorType: record.ActorType, ActorID: record.ActorID, Action: record.Action,
		ResourceType: record.ResourceType, ResourceID: record.ResourceID, RequestID: record.RequestID,
		TraceID: record.TraceID, Result: chainResultIntent, Reason: record.Reason,
	})
}
