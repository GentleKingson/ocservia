package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid  = errors.New("approval request is invalid")
	ErrSelf     = errors.New("requester cannot approve their own request")
	ErrNotReady = errors.New("approval is not valid for this action")
)

type Request struct {
	WorkspaceID, RequesterID, ResourceID uuid.UUID
	Action, ResourceType, Reason         string
	TTL                                  time.Duration
	SessionID                            uuid.UUID
	RequestID                            string
	RequestHash                          []byte
	RequestSummary                       json.RawMessage
}

type Decision struct {
	ApprovalID, ApproverID, SessionID uuid.UUID
	Reason                            string
	RequestID                         string
}

type Approval struct {
	ID             uuid.UUID       `json:"id"`
	WorkspaceID    uuid.UUID       `json:"workspace_id"`
	RequesterID    uuid.UUID       `json:"requester_id"`
	ApproverID     *uuid.UUID      `json:"approver_id,omitempty"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     uuid.UUID       `json:"resource_id"`
	Reason         string          `json:"reason"`
	Status         string          `json:"status"`
	ExpiresAt      time.Time       `json:"expires_at"`
	CreatedAt      time.Time       `json:"created_at"`
	RequestHash    string          `json:"request_hash,omitempty"`
	RequestSummary json.RawMessage `json:"request_summary,omitempty"`
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, request Request) (Approval, error) {
	request.Action = strings.TrimSpace(request.Action)
	request.ResourceType = strings.TrimSpace(request.ResourceType)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.WorkspaceID == uuid.Nil || request.RequesterID == uuid.Nil || request.ResourceID == uuid.Nil || request.SessionID == uuid.Nil || request.RequestID == "" ||
		request.Action == "" || len(request.Action) > 128 || request.ResourceType == "" || len(request.ResourceType) > 64 || request.Reason == "" || len(request.Reason) > 512 || request.TTL < time.Minute || request.TTL > 24*time.Hour ||
		(len(request.RequestHash) != 0 && len(request.RequestHash) != 32) || (len(request.RequestHash) == 0) != (len(request.RequestSummary) == 0) || (len(request.RequestSummary) != 0 && !json.Valid(request.RequestSummary)) {
		return Approval{}, ErrInvalid
	}
	now := s.now()
	approval := Approval{ID: uuid.Must(uuid.NewV7()), WorkspaceID: request.WorkspaceID, RequesterID: request.RequesterID, Action: request.Action, ResourceType: request.ResourceType, ResourceID: request.ResourceID, Reason: request.Reason, Status: "pending", ExpiresAt: now.Add(request.TTL), CreatedAt: now}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Approval{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if len(request.RequestHash) != 0 {
		approval.RequestHash = fmt.Sprintf("%x", request.RequestHash)
		approval.RequestSummary = request.RequestSummary
	}
	if _, err := tx.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8,$9,$10,$11)`, approval.ID, approval.WorkspaceID, approval.RequesterID, approval.Action, approval.ResourceType, approval.ResourceID, approval.Reason, approval.ExpiresAt, approval.CreatedAt, nullableBytes(request.RequestHash), nullableJSON(request.RequestSummary)); err != nil {
		return Approval{}, fmt.Errorf("insert approval request: %w", err)
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: approval.WorkspaceID, ActorType: "user", ActorID: approval.RequesterID.String(), SessionID: &request.SessionID, Action: "approval.request", ResourceType: "approval", ResourceID: approval.ID, ApprovalID: &approval.ID, RequestID: request.RequestID, Result: "intent", Reason: approval.Reason, AfterSummary: approvalSummary(approval), At: now}); err != nil {
		return Approval{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Approval{}, err
	}
	return approval, nil
}

func (s *Service) Approve(ctx context.Context, decision Decision) (Approval, error) {
	decision.Reason = strings.TrimSpace(decision.Reason)
	if decision.ApprovalID == uuid.Nil || decision.ApproverID == uuid.Nil || decision.SessionID == uuid.Nil || decision.RequestID == "" || decision.Reason == "" || len(decision.Reason) > 512 {
		return Approval{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Approval{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	approval, err := scan(tx.QueryRow(ctx, `SELECT id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,COALESCE(encode(request_hash,'hex'),''),request_summary FROM approval_requests WHERE id=$1 FOR UPDATE`, decision.ApprovalID))
	if err != nil {
		return Approval{}, err
	}
	if approval.RequesterID == decision.ApproverID {
		return Approval{}, ErrSelf
	}
	if approval.Status != "pending" || !approval.ExpiresAt.After(s.now()) {
		return Approval{}, ErrNotReady
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE approval_requests SET status='approved',approver_id=$2,approval_reason=$3,approved_at=$4 WHERE id=$1`, approval.ID, decision.ApproverID, decision.Reason, now); err != nil {
		return Approval{}, err
	}
	approval.Status, approval.ApproverID = "approved", &decision.ApproverID
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: approval.WorkspaceID, ActorType: "user", ActorID: decision.ApproverID.String(), SessionID: &decision.SessionID, Action: "approval.approve", ResourceType: "approval", ResourceID: approval.ID, ApprovalID: &approval.ID, RequestID: decision.RequestID, Result: "succeeded", Reason: decision.Reason, AfterSummary: approvalSummary(approval), At: now}); err != nil {
		return Approval{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Approval{}, err
	}
	return approval, nil
}

func Consume(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID) error {
	return consume(ctx, tx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, nil)
}

func ConsumeBound(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	return consume(ctx, tx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash)
}

func consume(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if approvalID == uuid.Nil {
		return ErrNotReady
	}
	query := `UPDATE approval_requests SET status='consumed',consumed_at=now()
		WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4 AND resource_type=$5 AND resource_id=$6
		  AND status='approved' AND approver_id IS DISTINCT FROM requester_id AND expires_at>now()`
	args := []any{approvalID, workspaceID, requesterID, action, resourceType, resourceID}
	if len(requestHash) != 0 {
		query += ` AND request_hash=$7`
		args = append(args, requestHash)
	}
	result, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("consume approval: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrNotReady
	}
	return nil
}

// ValidateConsumed accepts only the approval that authorized an already
// completed action. It makes idempotent retries prove the original approval
// without attempting to consume a second approval.
func ValidateConsumed(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID) error {
	return validateConsumed(ctx, tx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, nil)
}

func ValidateConsumedBound(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	return validateConsumed(ctx, tx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash)
}

func validateConsumed(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if approvalID == uuid.Nil {
		return ErrNotReady
	}
	var valid bool
	query := `SELECT EXISTS(
		SELECT 1 FROM approval_requests
		WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4
		  AND resource_type=$5 AND resource_id=$6 AND status='consumed'
		  AND approver_id IS DISTINCT FROM requester_id
	`
	args := []any{approvalID, workspaceID, requesterID, action, resourceType, resourceID}
	if len(requestHash) != 0 {
		query += ` AND request_hash=$7`
		args = append(args, requestHash)
	}
	query += `)`
	err := tx.QueryRow(ctx, query, args...).Scan(&valid)
	if err != nil {
		return fmt.Errorf("validate consumed approval: %w", err)
	}
	if !valid {
		return ErrNotReady
	}
	return nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Approval, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,COALESCE(encode(request_hash,'hex'),''),request_summary FROM approval_requests WHERE id=$1`, id))
}

func scan(row pgx.Row) (Approval, error) {
	var value Approval
	err := row.Scan(&value.ID, &value.WorkspaceID, &value.RequesterID, &value.ApproverID, &value.Action, &value.ResourceType, &value.ResourceID, &value.Reason, &value.Status, &value.ExpiresAt, &value.CreatedAt, &value.RequestHash, &value.RequestSummary)
	return value, err
}

func approvalSummary(value Approval) json.RawMessage {
	if value.RequestHash == "" {
		return nil
	}
	result, _ := json.Marshal(map[string]any{"request_hash": value.RequestHash, "request_summary": value.RequestSummary})
	return result
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
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
