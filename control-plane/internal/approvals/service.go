package approvals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	AuthorityResources                   []AuthorityResource
	BatchItems                           []BoundBatchItem
}

type BoundBatchItem struct {
	NodeID           uuid.UUID
	Username, Action string
	ExpectedVersion  int64
}

type AuthorityResource struct {
	WorkspaceID uuid.UUID
	Type        string
	ID          uuid.UUID
}

type Decision struct {
	ApprovalID, ApproverID, SessionID uuid.UUID
	Reason                            string
	RequestID                         string
	ExpectedRequestHash               string
}

type Approval struct {
	ID                 uuid.UUID       `json:"id"`
	WorkspaceID        uuid.UUID       `json:"workspace_id"`
	RequesterID        uuid.UUID       `json:"requester_id"`
	ApproverID         *uuid.UUID      `json:"approver_id,omitempty"`
	Action             string          `json:"action"`
	ResourceType       string          `json:"resource_type"`
	ResourceID         uuid.UUID       `json:"resource_id"`
	Reason             string          `json:"reason"`
	Status             string          `json:"status"`
	ExpiresAt          time.Time       `json:"expires_at"`
	CreatedAt          time.Time       `json:"created_at"`
	RequestHash        string          `json:"request_hash,omitempty"`
	RequestSummary     json.RawMessage `json:"request_summary,omitempty"`
	ConfigPlanSummary  json.RawMessage `json:"config_plan_summary,omitempty"`
	CertificateSummary json.RawMessage `json:"certificate_summary,omitempty"`
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

func GenericBinding(action, resourceType string, resourceID uuid.UUID) ([]byte, json.RawMessage) {
	summary, _ := json.Marshal(map[string]any{"action": action, "resource_type": resourceType, "resource_id": resourceID})
	digest := sha256.Sum256(append([]byte("ocservia/approval-request/v1\x00"), summary...))
	return digest[:], summary
}

// AgentUpgradeBinding binds an approval to the exact immutable agent release
// identity an upgrade may install. Approving a node upgrade therefore never
// authorizes a different version, package digest, or architecture than the
// approver reviewed.
func AgentUpgradeBinding(nodeID uuid.UUID, targetVersion string, packageSHA256 []byte, architecture string) ([]byte, json.RawMessage) {
	summary, _ := json.Marshal(map[string]any{"action": "agent.upgrade", "node_id": nodeID, "target_version": targetVersion, "package_sha256": hex.EncodeToString(packageSHA256), "architecture": architecture})
	digest := sha256.Sum256(append([]byte("ocservia/approval-request/agent-upgrade/v1\x00"), summary...))
	return digest[:], summary
}

// AgentRolloutBinding binds one approval to the exact immutable fleet
// rollout request: target version, the sorted node set, the batch size, and
// the stop-on-failure policy. The consumed approval authorizes exactly the
// reviewed rollout and nothing else; per-node eligibility and command
// authorization still apply at dispatch time.
func AgentRolloutBinding(targetVersion string, nodeIDs []uuid.UUID, batchSize int, stopOnFailure bool) ([]byte, json.RawMessage) {
	identifiers := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		identifiers = append(identifiers, nodeID.String())
	}
	summary, _ := json.Marshal(map[string]any{"action": "agent.rollout", "target_version": targetVersion, "node_ids": identifiers, "batch_size": batchSize, "stop_on_failure": stopOnFailure})
	digest := sha256.Sum256(append([]byte("ocservia/approval-request/agent-rollout/v1\x00"), summary...))
	return digest[:], summary
}

func (s *Service) Create(ctx context.Context, request Request) (Approval, error) {
	request.Action = strings.TrimSpace(request.Action)
	request.ResourceType = strings.TrimSpace(request.ResourceType)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.WorkspaceID == uuid.Nil || request.RequesterID == uuid.Nil || request.ResourceID == uuid.Nil || request.SessionID == uuid.Nil || request.RequestID == "" ||
		request.Action == "" || len(request.Action) > 128 || request.ResourceType == "" || len(request.ResourceType) > 64 || request.Reason == "" || len(request.Reason) > 512 || request.TTL < time.Minute || request.TTL > 24*time.Hour ||
		len(request.RequestHash) != 32 || len(request.RequestSummary) == 0 || !json.Valid(request.RequestSummary) || len(request.AuthorityResources) == 0 {
		return Approval{}, ErrInvalid
	}
	for _, resource := range request.AuthorityResources {
		if resource.WorkspaceID != request.WorkspaceID || !slices.Contains([]string{"workspace", "node", "resource", "secret_ref", "certificate", "config_plan", "batch_operation", "role_binding"}, resource.Type) || (resource.Type == "workspace") != (resource.ID == uuid.Nil) {
			return Approval{}, ErrInvalid
		}
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
		if request.ResourceType == "config_plan" {
			approval.ConfigPlanSummary = request.RequestSummary
		} else if request.ResourceType == "certificate" {
			approval.CertificateSummary = request.RequestSummary
		} else {
			approval.RequestSummary = request.RequestSummary
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary,authority_snapshot_at) VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8,$9,$10,$11,$9)`, approval.ID, approval.WorkspaceID, approval.RequesterID, approval.Action, approval.ResourceType, approval.ResourceID, approval.Reason, approval.ExpiresAt, approval.CreatedAt, request.RequestHash, request.RequestSummary); err != nil {
		return Approval{}, fmt.Errorf("insert approval request: %w", err)
	}
	for _, resource := range request.AuthorityResources {
		resourceID := resource.ID
		if resource.Type == "workspace" {
			resourceID = resource.WorkspaceID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO approval_authority_resources(approval_id,workspace_id,resource_type,resource_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, approval.ID, resource.WorkspaceID, resource.Type, resourceID); err != nil {
			return Approval{}, fmt.Errorf("record approval authority snapshot: %w", err)
		}
	}
	for index, item := range request.BatchItems {
		if _, err := tx.Exec(ctx, `INSERT INTO approval_batch_items(approval_id,item_index,node_id,username,action,expected_version) VALUES($1,$2,$3,$4,$5,$6)`, approval.ID, index, item.NodeID, item.Username, item.Action, item.ExpectedVersion); err != nil {
			return Approval{}, err
		}
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
	if approval.RequestHash != "" && decision.ExpectedRequestHash != approval.RequestHash {
		return Approval{}, ErrNotReady
	}
	if approval.Status != "pending" || !approval.ExpiresAt.After(s.now()) {
		return Approval{}, ErrNotReady
	}
	var authorized bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_authority_resources WHERE approval_id=$1) AND NOT EXISTS (
		SELECT 1 FROM approval_authority_resources scope
		WHERE scope.approval_id=$1 AND NOT EXISTS (
			SELECT 1 FROM role_bindings binding
			WHERE binding.identity_id=$2 AND binding.workspace_id=scope.workspace_id
			  AND binding.created_at <= (SELECT authority_snapshot_at FROM approval_requests WHERE id=$1)
			  AND binding.role_name IN ('SecurityAdmin','PlatformAdmin')
			  AND (binding.resource_type='workspace' OR (binding.resource_type=scope.resource_type AND binding.resource_id=scope.resource_id))
		)
	)`, approval.ID, decision.ApproverID).Scan(&authorized); err != nil {
		return Approval{}, err
	}
	if !authorized {
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

func ConsumeBound(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if len(requestHash) != sha256.Size {
		return ErrNotReady
	}
	return consume(ctx, tx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash)
}

func (s *Service) ValidateApprovedBound(ctx context.Context, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if len(requestHash) != sha256.Size {
		return ErrNotReady
	}
	var valid bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_requests WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4 AND resource_type=$5 AND resource_id=$6 AND request_hash=$7 AND status='approved' AND approver_id IS DISTINCT FROM requester_id AND expires_at>now())`, approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrNotReady
	}
	return nil
}

func consume(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if approvalID == uuid.Nil {
		return ErrNotReady
	}
	query := `UPDATE approval_requests SET status='consumed',consumed_at=now()
		WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4 AND resource_type=$5 AND resource_id=$6
		  AND status='approved' AND request_hash IS NOT NULL AND request_summary IS NOT NULL AND approver_id IS DISTINCT FROM requester_id AND expires_at>now()`
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

func ValidateConsumedBound(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if len(requestHash) != sha256.Size {
		return ErrNotReady
	}
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

func (s *Service) AuthorityResources(ctx context.Context, id uuid.UUID) ([]AuthorityResource, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id,resource_type,resource_id FROM approval_authority_resources WHERE approval_id=$1 ORDER BY resource_type,resource_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []AuthorityResource
	for rows.Next() {
		var value AuthorityResource
		if err := rows.Scan(&value.WorkspaceID, &value.Type, &value.ID); err != nil {
			return nil, err
		}
		if value.Type == "workspace" {
			value.ID = uuid.Nil
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func scan(row pgx.Row) (Approval, error) {
	var value Approval
	err := row.Scan(&value.ID, &value.WorkspaceID, &value.RequesterID, &value.ApproverID, &value.Action, &value.ResourceType, &value.ResourceID, &value.Reason, &value.Status, &value.ExpiresAt, &value.CreatedAt, &value.RequestHash, &value.RequestSummary)
	if err == nil && value.ResourceType == "config_plan" {
		value.ConfigPlanSummary = value.RequestSummary
		value.RequestSummary = nil
	} else if err == nil && value.ResourceType == "certificate" {
		value.CertificateSummary = value.RequestSummary
		value.RequestSummary = nil
	}
	return value, err
}

func approvalSummary(value Approval) json.RawMessage {
	if value.RequestHash == "" {
		return nil
	}
	summary := value.RequestSummary
	if len(summary) == 0 {
		summary = value.ConfigPlanSummary
	}
	if len(summary) == 0 {
		summary = value.CertificateSummary
	}
	result, _ := json.Marshal(map[string]any{"request_hash": value.RequestHash, "request_summary": summary})
	return result
}
