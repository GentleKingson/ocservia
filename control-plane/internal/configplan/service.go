package configplan

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	configurationstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrStaleRevision = operations.ErrStaleRevision
	ErrCapability    = errors.New("configuration planning capability is unavailable")
)

type CreateRequest struct {
	NodeID           uuid.UUID
	ExpectedRevision int64
	Template         Template
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
	MaterializedHash string            `json:"materialized_hash,omitempty"`
	State            string            `json:"state"`
	Validation       string            `json:"validation"`
	DiffRedacted     string            `json:"diff_redacted"`
	Warnings         []json.RawMessage `json:"warnings"`
	CurrentUnchanged bool              `json:"current_unchanged"`
	StagingCleaned   bool              `json:"staging_cleaned"`
	CurrentHash      string            `json:"-"`
	ExpiresAt        time.Time         `json:"expires_at"`
	CreatedAt        time.Time         `json:"created_at"`
}

type ApplyRequest struct {
	PlanID          uuid.UUID
	IdempotencyKey  string
	ActorID         string
	ActorIdentityID uuid.UUID
	ActorSessionID  uuid.UUID
	RequestID       string
	Traceparent     string
	Reason          string
}

type operationCreator interface {
	Create(context.Context, operations.CreateRequest) (operations.Operation, bool, error)
}

var _ operationCreator = (*operations.Service)(nil)

type Service struct {
	backend    database.Backend
	operations operationCreator
	now        func() time.Time
}

func New(backend database.Backend, operationService operationCreator) *Service {
	return &Service{backend: backend, operations: operationService, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Plan, bool, error) {
	if request.NodeID == uuid.Nil || request.ExpectedRevision < 0 || request.TTL < time.Minute || request.TTL > time.Hour || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 {
		return Plan{}, false, ErrInvalid
	}
	var node configurationstore.Node
	var hydrated Template
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		hydrated = request.Template
		hydrated.Directives = slices.Clone(request.Template.Directives)
		var err error
		node, err = configurationstore.FromTransaction(tx).Node(ctx, request.NodeID)
		if err != nil {
			return err
		}
		for index := range hydrated.Directives {
			ref := hydrated.Directives[index].SecretRef
			if ref == nil {
				continue
			}
			if ref.ID == uuid.Nil || ref.Provider != "" || ref.Key != "" || ref.Version != "" {
				return ErrInvalid
			}
			stored, err := certificatestore.FromTransaction(tx).GetSecretReference(ctx, ref.ID)
			if errors.Is(err, database.ErrNotFound) {
				return ErrInvalid
			}
			if err != nil {
				return err
			}
			if stored.WorkspaceID != node.WorkspaceID || stored.State != "active" {
				return ErrInvalid
			}
			resolved := *ref
			resolved.Provider, resolved.Key, resolved.Version = stored.Provider, stored.KeyPath, stored.Version
			hydrated.Directives[index].SecretRef = &resolved
		}
		return nil
	})
	if err != nil {
		return Plan{}, false, err
	}
	rendered, err := renderComplete(RenderInput{Template: hydrated, OcservVersion: node.OcservVersion}, request.NodeID, uint64(request.ExpectedRevision))
	if err != nil {
		return Plan{}, false, err
	}
	op, replayed, err := s.operations.Create(ctx, operations.CreateRequest{
		NodeID:          request.NodeID,
		IdempotencyKey:  request.IdempotencyKey,
		ExpectedVersion: node.Version,
		TTL:             request.TTL,
		RequestID:       request.RequestID,
		Traceparent:     request.Traceparent,
		ActorID:         request.ActorID,
		ActorIdentityID: request.ActorIdentityID,
		ActorSessionID:  request.ActorSessionID,
		Action:          "config.plan",
		Reason:          request.Reason,
		Payload: operations.ConfigPlanPayload{
			Candidate:     rendered,
			OcservVersion: node.OcservVersion,
			PlanMetadata:  &operations.ConfigPlanMetadata{TemplateName: request.Template.Name, CandidateRedacted: rendered.Redacted(), Warnings: []string{}, CreatedBy: request.ActorIdentityID},
		},
	})
	if errors.Is(err, operations.ErrCapabilityMissing) {
		return Plan{}, false, ErrCapability
	}
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
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		stored, err = configurationstore.FromTransaction(tx).Get(ctx, id)
		return err
	})
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{ID: stored.ID, WorkspaceID: stored.WorkspaceID, NodeID: stored.NodeID, OperationID: stored.OperationID, TemplateName: stored.TemplateName, ExpectedRevision: stored.ExpectedRevision, State: stored.State, CandidateHash: hex.EncodeToString(stored.CandidateHash), ExpiresAt: stored.ExpiresAt.UTC(), CreatedAt: stored.CreatedAt.UTC()}
	if err := json.Unmarshal(stored.Warnings, &plan.Warnings); err != nil {
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
				if strings.HasPrefix(stored.CandidateRedacted, "# generated by ocservia complete-config/v1\n") {
					if len(validation.GetMaterializedHash()) != 32 || len(validation.GetWarnings()) != 0 {
						plan.Validation = "failed"
					} else {
						plan.MaterializedHash = hex.EncodeToString(validation.GetMaterializedHash())
					}
				} else if len(validation.GetMaterializedHash()) != 0 {
					plan.Validation = "failed"
				}
			} else {
				plan.Validation = "failed"
			}
		}
	}
	return plan, nil
}

// Apply queues the exact validated candidate under the caller's authority.
func (s *Service) Apply(ctx context.Context, request ApplyRequest) (operations.Operation, bool, error) {
	if request.PlanID == uuid.Nil || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 {
		return operations.Operation{}, false, ErrInvalid
	}
	plan, err := s.Get(ctx, request.PlanID)
	if err != nil {
		return operations.Operation{}, false, err
	}
	if plan.Validation != "valid" || plan.CurrentHash == "" || plan.ExpiresAt.IsZero() || !plan.ExpiresAt.After(s.now()) {
		return operations.Operation{}, false, ErrStaleRevision
	}
	var input configurationstore.ApplyInput
	if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		input, err = configurationstore.FromTransaction(tx).ApplyInput(ctx, request.PlanID, request.IdempotencyKey)
		return err
	}); err != nil {
		return operations.Operation{}, false, err
	}
	var envelope agentv1.CommandEnvelope
	if proto.Unmarshal(input.Envelope, &envelope) != nil {
		return operations.Operation{}, false, ErrInvalid
	}
	complete := envelope.GetCompleteConfigPlan()
	if complete == nil {
		return operations.Operation{}, false, ErrInvalid
	}
	candidate, err := configprofile.Normalize(complete.GetCandidate())
	if err != nil || !bytes.Equal(candidate.Digest(), complete.GetCandidateHash()) || candidate.Revision() != uint64(plan.ExpectedRevision) {
		return operations.Operation{}, false, ErrInvalid
	}
	materialized, err := hex.DecodeString(plan.MaterializedHash)
	if err != nil || len(materialized) != 32 {
		return operations.Operation{}, false, ErrInvalid
	}
	expires := plan.ExpiresAt.UTC()
	previousHash, err := hex.DecodeString(plan.CurrentHash)
	if err != nil || len(previousHash) != 32 {
		return operations.Operation{}, false, ErrInvalid
	}
	return s.operations.Create(ctx, operations.CreateRequest{
		NodeID:          plan.NodeID,
		IdempotencyKey:  request.IdempotencyKey,
		ExpectedVersion: input.NodeVersion,
		TTL:             15 * time.Minute,
		RequestID:       request.RequestID,
		Traceparent:     request.Traceparent,
		ActorID:         request.ActorID,
		ActorIdentityID: request.ActorIdentityID,
		ActorSessionID:  request.ActorSessionID,
		Action:          "config.apply",
		Reason:          request.Reason,
		Payload: operations.ConfigApplyPayload{
			Candidate:           candidate,
			ExpectedCurrentHash: previousHash,
			MaterializedHash:    materialized,
			DesiredRevision:     uint64(input.DesiredRevision) + 1,
			PlanExpiresAt:       timestamppb.New(expires),
			ApplyMetadata:       &operations.ConfigApplyMetadata{PlanID: request.PlanID},
		},
	})
}

func (s *Service) Resource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		workspaceID, nodeID, err = configurationstore.FromTransaction(tx).Resource(ctx, id)
		return err
	})
	return
}

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
