package configplan

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	configurationstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var (
	ErrStaleRevision = operations.ErrStaleRevision
	ErrCapability    = errors.New("configuration planning capability is unavailable")
	ErrIdempotency   = operations.ErrIdempotencyConflict
)

type CreateRequest struct {
	NodeID           uuid.UUID
	ExpectedRevision int64
	Template         Template
	NodeVariables    map[string]string
	TTL              time.Duration
	IdempotencyKey   string
	ActorID          string
	ActorIdentityID  uuid.UUID
	ActorSessionID   uuid.UUID
	RequestID        string
	Traceparent      string
	Reason           string
}

type Plan struct {
	ID               uuid.UUID         `json:"id"`
	WorkspaceID      uuid.UUID         `json:"workspace_id"`
	NodeID           uuid.UUID         `json:"node_id"`
	OperationID      uuid.UUID         `json:"operation_id"`
	TemplateName     string            `json:"template_name"`
	ExpectedRevision int64             `json:"expected_revision"`
	CandidateHash    string            `json:"candidate_hash"`
	State            string            `json:"state"`
	Validation       string            `json:"validation"`
	DiffRedacted     string            `json:"diff_redacted"`
	Warnings         []json.RawMessage `json:"warnings"`
	CurrentUnchanged bool              `json:"current_unchanged"`
	StagingCleaned   bool              `json:"staging_cleaned"`
	CurrentHash      string            `json:"current_hash,omitempty"`
	ApprovalID       *uuid.UUID        `json:"approval_id,omitempty"`
	ApprovalStatus   string            `json:"approval_status,omitempty"`
	ExpiresAt        value.Timestamp   `json:"expires_at"`
	CreatedAt        value.Timestamp   `json:"created_at"`
}

type ApplyRequest struct {
	PlanID          uuid.UUID
	ApprovalID      uuid.UUID
	IdempotencyKey  string
	ActorID         string
	ActorIdentityID uuid.UUID
	ActorSessionID  uuid.UUID
	RequestID       string
	Traceparent     string
	Reason          string
}

type Service struct {
	backend    database.Backend
	operations *operations.Service
	now        func() time.Time
}

func New(pool *pgxpool.Pool, operationService *operations.Service) *Service {
	return NewBackend(postgres.WrapPool(pool), operationService)
}

func NewBackend(backend database.Backend, operationService *operations.Service) *Service {
	return &Service{backend: backend, operations: operationService, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Plan, bool, error) {
	if request.NodeID == uuid.Nil || request.ExpectedRevision < 0 || request.TTL < time.Minute || request.TTL > time.Hour || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 {
		return Plan{}, false, ErrInvalid
	}
	var node configurationstore.Node
	var capabilities []string
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		node, err = store.Node(ctx, request.NodeID)
		if err != nil {
			return err
		}
		capabilities, err = store.Capabilities(ctx, request.NodeID)
		if err != nil {
			return err
		}
		hasPlan := false
		for _, capability := range capabilities {
			if capability == "ocserv.config.plan" {
				hasPlan = true
			}
		}
		if !hasPlan {
			return ErrCapability
		}
		for index := range request.Template.Directives {
			ref := request.Template.Directives[index].SecretRef
			if ref == nil {
				continue
			}
			if ref.ID == uuid.Nil || ref.Provider != "" || ref.Key != "" || ref.Version != "" {
				return ErrInvalid
			}
			secrets, err := certificatestore.FromTransaction(tx)
			if err != nil {
				return err
			}
			stored, err := secrets.GetSecretReference(ctx, ref.ID)
			if errors.Is(err, database.ErrNotFound) {
				return ErrInvalid
			}
			if err != nil {
				return err
			}
			if stored.WorkspaceID != node.WorkspaceID || stored.State != "active" {
				return ErrInvalid
			}
			ref.Provider, ref.Key, ref.Version = stored.Provider, stored.KeyPath, stored.Version
		}
		return nil
	})
	if err != nil {
		return Plan{}, false, err
	}
	rendered, err := Render(RenderInput{Template: request.Template, NodeVariables: request.NodeVariables, OcservVersion: node.OcservVersion, Capabilities: capabilities})
	if err != nil {
		return Plan{}, false, err
	}
	op, replayed, err := s.operations.CreateSynthetic(ctx, operations.CreateRequest{
		NodeID: request.NodeID, IdempotencyKey: request.IdempotencyKey, ExpectedVersion: node.Version,
		Kind: operations.ConfigPlan, Candidate: rendered.Candidate, CandidateHash: rendered.Hash[:], PlanRevision: uint64(request.ExpectedRevision),
		TTL: request.TTL, RequestID: request.RequestID, Traceparent: request.Traceparent,
		ActorID: request.ActorID, ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID,
		Action: "config.plan", Reason: request.Reason,
		OcservVersion: node.OcservVersion, PlanCapabilities: rendered.RequiredCapabilities,
		PlanMetadata: &operations.ConfigPlanMetadata{TemplateName: request.Template.Name, CandidateRedacted: rendered.Redacted, Warnings: rendered.Warnings, CreatedBy: request.ActorIdentityID},
	})
	if err != nil {
		return Plan{}, false, err
	}
	planID, err := uuid.Parse(op.ID)
	if err != nil {
		return Plan{}, false, err
	}
	plan, err := s.Get(ctx, planID)
	return plan, replayed, err
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Plan, error) {
	var stored configurationstore.Plan
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		stored, err = store.Get(ctx, id)
		return err
	})
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{ID: stored.ID, WorkspaceID: stored.WorkspaceID, NodeID: stored.NodeID, OperationID: stored.OperationID, TemplateName: stored.TemplateName, ExpectedRevision: stored.ExpectedRevision, State: stored.State, CandidateHash: hex.EncodeToString(stored.CandidateHash), ExpiresAt: stored.ExpiresAt, CreatedAt: stored.CreatedAt, ApprovalID: stored.ApprovalID, ApprovalStatus: stored.ApprovalStatus}
	if err := json.Unmarshal(stored.Warnings.Bytes(), &plan.Warnings); err != nil {
		return Plan{}, err
	}
	expectedDiff := safeDiff(stored.CandidateRedacted)
	plan.Validation = "pending"
	if plan.State == "failed" || plan.State == "rejected" || plan.State == "unknown" || plan.State == "expired" {
		plan.Validation = plan.State
	}
	if len(stored.Result) != 0 && plan.State == "succeeded" {
		var validation agentv1.ConfigPlanResult
		if proto.Unmarshal(stored.Result, &validation) != nil {
			plan.Validation = "failed"
		} else {
			if bytes.Equal(validation.GetCandidateHash(), stored.CandidateHash) && validation.GetDiffRedacted() == expectedDiff && safeValidationWarnings(validation.GetWarnings()) && validation.GetCurrentUnchanged() && validation.GetStagingCleaned() {
				plan.Validation = "valid"
				plan.DiffRedacted = expectedDiff
				for _, warning := range validation.GetWarnings() {
					encoded, _ := json.Marshal(warning)
					plan.Warnings = append(plan.Warnings, encoded)
				}
				plan.CurrentUnchanged = true
				plan.StagingCleaned = true
				if len(validation.GetCurrentHash()) == 32 {
					plan.CurrentHash = hex.EncodeToString(validation.GetCurrentHash())
				} else {
					plan.Validation = "failed"
				}
			} else {
				plan.Validation = "failed"
			}
		}
	}
	return plan, nil
}

// Apply atomically consumes the independent approval and queues the exact validated candidate.
func (s *Service) Apply(ctx context.Context, request ApplyRequest) (operations.Operation, bool, error) {
	if request.PlanID == uuid.Nil || request.ApprovalID == uuid.Nil || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 {
		return operations.Operation{}, false, ErrInvalid
	}
	plan, err := s.Get(ctx, request.PlanID)
	if err != nil {
		return operations.Operation{}, false, err
	}
	now, err := value.FromTime(s.now())
	if err != nil {
		return operations.Operation{}, false, err
	}
	if plan.Validation != "valid" || plan.CurrentHash == "" || !plan.ExpiresAt.Valid || plan.ExpiresAt.Micros <= now.Micros {
		return operations.Operation{}, false, ErrStaleRevision
	}
	var input configurationstore.ApplyInput
	if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		input, err = store.ApplyInput(ctx, request.PlanID)
		return err
	}); err != nil {
		return operations.Operation{}, false, err
	}
	var envelope agentv1.CommandEnvelope
	if proto.Unmarshal(input.Envelope, &envelope) != nil || envelope.GetConfigPlan() == nil {
		return operations.Operation{}, false, ErrInvalid
	}
	previousHash, err := hex.DecodeString(plan.CurrentHash)
	if err != nil || len(previousHash) != 32 {
		return operations.Operation{}, false, ErrInvalid
	}
	return s.operations.CreateSynthetic(ctx, operations.CreateRequest{
		NodeID: plan.NodeID, IdempotencyKey: request.IdempotencyKey, ExpectedVersion: input.NodeVersion,
		Kind: operations.ConfigApply, Candidate: envelope.GetConfigPlan().GetCandidate(), CandidateHash: envelope.GetConfigPlan().GetCandidateHash(),
		ExpectedCurrentHash: previousHash, PlanRevision: uint64(plan.ExpectedRevision), DesiredRevision: uint64(input.DesiredRevision) + 1,
		ApplyMetadata: &operations.ConfigApplyMetadata{PlanID: request.PlanID}, ApprovalID: request.ApprovalID,
		TTL: 15 * time.Minute, RequestID: request.RequestID, Traceparent: request.Traceparent,
		ActorID: request.ActorID, ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID,
		Action: "config.apply", Reason: request.Reason,
	})
}

func (s *Service) Resource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		workspaceID, nodeID, err = store.Resource(ctx, id)
		return err
	})
	return
}

func IsNotFound(err error) bool { return errors.Is(err, database.ErrNotFound) }

func safeDiff(redactedCandidate string) string {
	var diff strings.Builder
	diff.WriteString("- <current configuration redacted>\n")
	for _, line := range strings.Split(strings.TrimSuffix(redactedCandidate, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		diff.WriteString("+ ")
		diff.WriteString(line)
		diff.WriteByte('\n')
	}
	return diff.String()
}

func safeValidationWarnings(warnings []string) bool {
	if len(warnings) > 4 {
		return false
	}
	for _, warning := range warnings {
		if warning != "secret_references_unresolved" {
			return false
		}
	}
	return true
}
