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

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvalstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
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
	backend database.Backend
	now     func() time.Time
}

func New(pool *pgxpool.Pool) *Service {
	return NewBackend(postgres.WrapPool(pool))
}

func NewBackend(backend database.Backend) *Service {
	return &Service{backend: backend, now: func() time.Time { return time.Now().UTC() }}
}

func store(source database.Store) (approvalstore.Store, error) {
	p, ok := source.(approvalstore.Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.ApprovalStore(), nil
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
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		data, err := store(tx)
		if err != nil {
			return err
		}
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
		if err := data.Insert(ctx, []any{approval.ID, approval.WorkspaceID, approval.RequesterID, approval.Action, approval.ResourceType, approval.ResourceID, approval.Reason, approval.ExpiresAt, approval.CreatedAt, request.RequestHash, request.RequestSummary}); err != nil {
			return fmt.Errorf("insert approval request: %w", err)
		}
		for _, resource := range request.AuthorityResources {
			resourceID := resource.ID
			if resource.Type == "workspace" {
				resourceID = resource.WorkspaceID
			}
			if err := data.AddAuthority(ctx, approval.ID, resource.WorkspaceID, resource.Type, resourceID); err != nil {
				return fmt.Errorf("record approval authority snapshot: %w", err)
			}
		}
		for index, item := range request.BatchItems {
			if err := data.AddBatchItem(ctx, approval.ID, index, item.NodeID, item.Username, item.Action, item.ExpectedVersion); err != nil {
				return err
			}
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: approval.WorkspaceID, ActorType: "user", ActorID: approval.RequesterID.String(), SessionID: &request.SessionID, Action: "approval.request", ResourceType: "approval", ResourceID: approval.ID, ApprovalID: &approval.ID, RequestID: request.RequestID, Result: "intent", Reason: approval.Reason, AfterSummary: approvalSummary(approval), At: now})
	})
	if err != nil {
		return Approval{}, err
	}
	return approval, nil
}

func (s *Service) Approve(ctx context.Context, decision Decision) (Approval, error) {
	decision.Reason = strings.TrimSpace(decision.Reason)
	if decision.ApprovalID == uuid.Nil || decision.ApproverID == uuid.Nil || decision.SessionID == uuid.Nil || decision.RequestID == "" || decision.Reason == "" || len(decision.Reason) > 512 {
		return Approval{}, ErrInvalid
	}
	var approval Approval
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		data, err := store(tx)
		if err != nil {
			return err
		}
		approval, err = scan(data.Get(ctx, decision.ApprovalID, true))
		if err != nil {
			return err
		}
		if approval.RequesterID == decision.ApproverID {
			return ErrSelf
		}
		if approval.RequestHash != "" && decision.ExpectedRequestHash != approval.RequestHash {
			return ErrNotReady
		}
		if approval.Status != "pending" || !approval.ExpiresAt.After(s.now()) {
			return ErrNotReady
		}
		authorized, err := data.Authorized(ctx, approval.ID, decision.ApproverID)
		if err != nil {
			return err
		}
		if !authorized {
			return ErrNotReady
		}
		now := s.now()
		if err := data.Approve(ctx, approval.ID, decision.ApproverID, decision.Reason, now); err != nil {
			return err
		}
		approval.Status, approval.ApproverID = "approved", &decision.ApproverID
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: approval.WorkspaceID, ActorType: "user", ActorID: decision.ApproverID.String(), SessionID: &decision.SessionID, Action: "approval.approve", ResourceType: "approval", ResourceID: approval.ID, ApprovalID: &approval.ID, RequestID: decision.RequestID, Result: "succeeded", Reason: decision.Reason, AfterSummary: approvalSummary(approval), At: now})
	})
	if err != nil {
		return Approval{}, err
	}
	return approval, nil
}

func ConsumeBound(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	return ConsumeBoundTx(ctx, postgres.WrapTx(tx), approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash)
}

func ConsumeBoundTx(ctx context.Context, tx database.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if len(requestHash) != sha256.Size || approvalID == uuid.Nil {
		return ErrNotReady
	}
	p, ok := tx.(approvalstore.Provider)
	if !ok {
		return database.ErrUnsupported
	}
	consumed, err := p.ApprovalStore().ConsumeBound(ctx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash)
	if err != nil {
		return fmt.Errorf("consume approval: %w", err)
	}
	if !consumed {
		return ErrNotReady
	}
	return nil
}

func (s *Service) ValidateApprovedBound(ctx context.Context, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if len(requestHash) != sha256.Size {
		return ErrNotReady
	}
	data, err := store(s.backend)
	if err != nil {
		return err
	}
	valid, err := data.ValidBound(ctx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash, false)
	if err != nil {
		return err
	}
	if !valid {
		return ErrNotReady
	}
	return nil
}

func ValidateConsumedBound(ctx context.Context, tx pgx.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	return ValidateConsumedBoundTx(ctx, postgres.WrapTx(tx), approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash)
}

func ValidateConsumedBoundTx(ctx context.Context, tx database.Tx, approvalID, workspaceID, requesterID uuid.UUID, action, resourceType string, resourceID uuid.UUID, requestHash []byte) error {
	if len(requestHash) != sha256.Size {
		return ErrNotReady
	}
	data, err := store(tx)
	if err != nil {
		return err
	}
	valid, err := data.ValidBound(ctx, approvalID, workspaceID, requesterID, action, resourceType, resourceID, requestHash, true)
	if err != nil {
		return err
	}
	if !valid {
		return ErrNotReady
	}
	return nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Approval, error) {
	data, err := store(s.backend)
	if err != nil {
		return Approval{}, err
	}
	return scan(data.Get(ctx, id, false))
}

func (s *Service) AuthorityResources(ctx context.Context, id uuid.UUID) ([]AuthorityResource, error) {
	data, err := store(s.backend)
	if err != nil {
		return nil, err
	}
	rows, err := data.AuthorityResources(ctx, id)
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

func scan(row database.Row) (Approval, error) {
	var record Approval
	var expires, created value.Timestamp
	err := row.Scan(&record.ID, &record.WorkspaceID, &record.RequesterID, &record.ApproverID, &record.Action, &record.ResourceType, &record.ResourceID, &record.Reason, &record.Status, &expires, &created, &record.RequestHash, &record.RequestSummary)
	if err == nil {
		record.ExpiresAt, err = expires.Time()
	}
	if err == nil {
		record.CreatedAt, err = created.Time()
	}
	if err == nil && record.ResourceType == "config_plan" {
		record.ConfigPlanSummary = record.RequestSummary
		record.RequestSummary = nil
	} else if err == nil && record.ResourceType == "certificate" {
		record.CertificateSummary = record.RequestSummary
		record.RequestSummary = nil
	}
	return record, err
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
