package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const chainResultIntent = "intent"

type ChainRecord struct {
	EventID                                  uuid.UUID
	WorkspaceID                              uuid.UUID
	ActorType, ActorID, Action, ResourceType string
	ResourceID                               uuid.UUID
	RequestID, TraceID, Reason               string
	Result, ErrorType                        string
	SessionID, NodeID, CommandID, ApprovalID *uuid.UUID
	BeforeSummary, AfterSummary              json.RawMessage
	At                                       time.Time
}

type Verification struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Events      int64     `json:"events"`
	Valid       bool      `json:"valid"`
	Checkpoint  bool      `json:"checkpoint_valid"`
}

type Manager struct {
	pool *pgxpool.Pool
	key  []byte
}

func NewManager(pool *pgxpool.Pool, checkpointKey []byte) *Manager {
	return &Manager{pool: pool, key: append([]byte(nil), checkpointKey...)}
}

func LockChain(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, workspaceID.String()); err != nil {
		return fmt.Errorf("lock audit chain: %w", err)
	}
	return nil
}

func AppendChain(ctx context.Context, tx pgx.Tx, record ChainRecord) error {
	if record.Result == "" {
		record.Result = chainResultIntent
	}
	if record.Result != "intent" && record.Result != "succeeded" && record.Result != "failed" {
		return errors.New("invalid audit result")
	}
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
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (id,workspace_id,occurred_at,actor_type,actor_id,source_session_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,approval_id,result,reason,before_summary,after_summary,error_type,previous_event_hash,event_hash) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`, record.EventID, record.WorkspaceID, record.At, record.ActorType, record.ActorID, record.SessionID, record.Action, record.ResourceType, nullableID(record.ResourceID), record.NodeID, record.RequestID, traceID, record.CommandID, record.ApprovalID, record.Result, nullableString(record.Reason), nullableJSON(record.BeforeSummary), nullableJSON(record.AfterSummary), nullableString(record.ErrorType), previous, digest[:])
	if err != nil {
		return fmt.Errorf("append audit intent: %w", err)
	}
	return nil
}

func encodeChainPayload(previous []byte, record ChainRecord) ([]byte, error) {
	before, err := canonicalJSON(record.BeforeSummary)
	if err != nil {
		return nil, err
	}
	after, err := canonicalJSON(record.AfterSummary)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Previous                                 []byte          `json:"previous"`
		EventID                                  uuid.UUID       `json:"event_id"`
		WorkspaceID                              uuid.UUID       `json:"workspace_id"`
		OccurredAt                               time.Time       `json:"occurred_at"`
		ActorType                                string          `json:"actor_type"`
		ActorID                                  string          `json:"actor_id"`
		Action                                   string          `json:"action"`
		ResourceType                             string          `json:"resource_type"`
		ResourceID                               uuid.UUID       `json:"resource_id"`
		RequestID                                string          `json:"request_id"`
		TraceID                                  string          `json:"trace_id"`
		Result                                   string          `json:"result"`
		Reason                                   string          `json:"reason"`
		SessionID, NodeID, CommandID, ApprovalID *uuid.UUID      `json:",omitempty"`
		BeforeSummary, AfterSummary              json.RawMessage `json:",omitempty"`
		ErrorType                                string          `json:"error_type,omitempty"`
	}{
		Previous: previous, EventID: record.EventID, WorkspaceID: record.WorkspaceID, OccurredAt: record.At.UTC(),
		ActorType: record.ActorType, ActorID: record.ActorID, Action: record.Action,
		ResourceType: record.ResourceType, ResourceID: record.ResourceID, RequestID: record.RequestID,
		TraceID: record.TraceID, Result: record.Result, Reason: record.Reason,
		SessionID: record.SessionID, NodeID: record.NodeID, CommandID: record.CommandID, ApprovalID: record.ApprovalID,
		BeforeSummary: before, AfterSummary: after, ErrorType: record.ErrorType,
	})
}

func canonicalJSON(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 || string(value) == "null" {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(decoded)
	return json.RawMessage(encoded), err
}

func (m *Manager) Verify(ctx context.Context, workspaceID uuid.UUID) (Verification, error) {
	rows, err := m.pool.Query(ctx, `SELECT id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,COALESCE(trace_id,''),result,COALESCE(reason,''),source_session_id,node_id,command_id,approval_id,before_summary,after_summary,COALESCE(error_type,''),previous_event_hash,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at,id`, workspaceID)
	if err != nil {
		return Verification{}, err
	}
	defer rows.Close()
	verification := Verification{WorkspaceID: workspaceID, Valid: true, Checkpoint: true}
	var previous []byte
	for rows.Next() {
		record := ChainRecord{WorkspaceID: workspaceID}
		var resourceID *uuid.UUID
		var storedPrevious, storedHash []byte
		if err := rows.Scan(&record.EventID, &record.At, &record.ActorType, &record.ActorID, &record.Action, &record.ResourceType, &resourceID, &record.RequestID, &record.TraceID, &record.Result, &record.Reason, &record.SessionID, &record.NodeID, &record.CommandID, &record.ApprovalID, &record.BeforeSummary, &record.AfterSummary, &record.ErrorType, &storedPrevious, &storedHash); err != nil {
			return Verification{}, err
		}
		if resourceID != nil {
			record.ResourceID = *resourceID
		}
		payload, err := encodeChainPayload(previous, record)
		if err != nil {
			return Verification{}, err
		}
		digest := sha256.Sum256(payload)
		if subtle.ConstantTimeCompare(storedPrevious, previous) != 1 || subtle.ConstantTimeCompare(storedHash, digest[:]) != 1 {
			verification.Valid = false
		}
		previous = append(previous[:0], storedHash...)
		verification.Events++
	}
	if err := rows.Err(); err != nil {
		return Verification{}, err
	}
	if len(m.key) == 0 {
		verification.Checkpoint = false
		return verification, nil
	}
	var checkpointHash, signature []byte
	err = m.pool.QueryRow(ctx, `SELECT through_event_hash,signature FROM audit_checkpoints WHERE workspace_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, workspaceID).Scan(&checkpointHash, &signature)
	if errors.Is(err, pgx.ErrNoRows) {
		verification.Checkpoint = verification.Events == 0
		return verification, nil
	}
	if err != nil {
		return Verification{}, err
	}
	verification.Checkpoint = hmac.Equal(signature, signCheckpoint(m.key, workspaceID, checkpointHash))
	return verification, nil
}

func (m *Manager) Checkpoint(ctx context.Context, workspaceID uuid.UUID) error {
	if len(m.key) < 32 {
		return errors.New("audit checkpoint key is unavailable")
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := LockChain(ctx, tx, workspaceID); err != nil {
		return err
	}
	var eventID uuid.UUID
	var eventHash []byte
	if err := tx.QueryRow(ctx, `SELECT id,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspaceID).Scan(&eventID, &eventHash); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_checkpoints(id,workspace_id,through_event_id,through_event_hash,signature,created_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(workspace_id,through_event_id) DO NOTHING`, uuid.Must(uuid.NewV7()), workspaceID, eventID, eventHash, signCheckpoint(m.key, workspaceID, eventHash))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (m *Manager) CheckpointAll(ctx context.Context) error {
	if len(m.key) == 0 {
		return nil
	}
	rows, err := m.pool.Query(ctx, `SELECT DISTINCT workspace_id FROM audit_events`)
	if err != nil {
		return err
	}
	var workspaces []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		workspaces = append(workspaces, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range workspaces {
		if err := m.Checkpoint(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func signCheckpoint(key []byte, workspaceID uuid.UUID, hash []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(workspaceID[:])
	_, _ = mac.Write(hash)
	return mac.Sum(nil)
}

func nullableID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
