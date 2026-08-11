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
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	ID               uuid.UUID  `json:"id"`
	WorkspaceID      uuid.UUID  `json:"workspace_id"`
	NodeID           uuid.UUID  `json:"node_id"`
	OperationID      uuid.UUID  `json:"operation_id"`
	TemplateName     string     `json:"template_name"`
	ExpectedRevision int64      `json:"expected_revision"`
	CandidateHash    string     `json:"candidate_hash"`
	State            string     `json:"state"`
	Validation       string     `json:"validation"`
	DiffRedacted     string     `json:"diff_redacted"`
	Warnings         []string   `json:"warnings"`
	CurrentUnchanged bool       `json:"current_unchanged"`
	StagingCleaned   bool       `json:"staging_cleaned"`
	CurrentHash      string     `json:"current_hash,omitempty"`
	ApprovalID       *uuid.UUID `json:"approval_id,omitempty"`
	ApprovalStatus   string     `json:"approval_status,omitempty"`
	ExpiresAt        time.Time  `json:"expires_at"`
	CreatedAt        time.Time  `json:"created_at"`
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
	pool       *pgxpool.Pool
	operations *operations.Service
	now        func() time.Time
}

func New(pool *pgxpool.Pool, operationService *operations.Service) *Service {
	return &Service{pool: pool, operations: operationService, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Plan, bool, error) {
	if request.NodeID == uuid.Nil || request.ExpectedRevision < 0 || request.TTL < time.Minute || request.TTL > time.Hour || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 {
		return Plan{}, false, ErrInvalid
	}
	var workspaceID uuid.UUID
	var nodeVersion int64
	var ocservVersion string
	if err := s.pool.QueryRow(ctx, `
		SELECT n.workspace_id,n.version,COALESCE((SELECT o.ocserv_version FROM node_observed_snapshots o WHERE o.node_id=n.id ORDER BY o.observed_at DESC LIMIT 1),'')
		FROM nodes n WHERE n.id=$1 AND n.status IN('active','offline')`, request.NodeID).
		Scan(&workspaceID, &nodeVersion, &ocservVersion); err != nil {
		return Plan{}, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT capability FROM node_capabilities WHERE node_id=$1 AND approved=true AND (capability='ocserv.config.plan' OR capability LIKE 'config.%') ORDER BY capability`, request.NodeID)
	if err != nil {
		return Plan{}, false, err
	}
	var capabilities []string
	for rows.Next() {
		var capability string
		if err := rows.Scan(&capability); err != nil {
			rows.Close()
			return Plan{}, false, err
		}
		capabilities = append(capabilities, capability)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Plan{}, false, err
	}
	hasPlan := false
	for _, capability := range capabilities {
		if capability == "ocserv.config.plan" {
			hasPlan = true
		}
	}
	if !hasPlan {
		return Plan{}, false, ErrCapability
	}
	for index := range request.Template.Directives {
		ref := request.Template.Directives[index].SecretRef
		if ref == nil {
			continue
		}
		if ref.ID == uuid.Nil || ref.Provider != "" || ref.Key != "" || ref.Version != "" {
			return Plan{}, false, ErrInvalid
		}
		if err := s.pool.QueryRow(ctx, `SELECT provider,key_path,version FROM secret_provider_refs WHERE id=$1 AND workspace_id=$2 AND state='active'`, ref.ID, workspaceID).Scan(&ref.Provider, &ref.Key, &ref.Version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Plan{}, false, ErrInvalid
			}
			return Plan{}, false, err
		}
	}
	rendered, err := Render(RenderInput{Template: request.Template, NodeVariables: request.NodeVariables, OcservVersion: ocservVersion, Capabilities: capabilities})
	if err != nil {
		return Plan{}, false, err
	}
	op, replayed, err := s.operations.CreateSynthetic(ctx, operations.CreateRequest{
		NodeID: request.NodeID, IdempotencyKey: request.IdempotencyKey, ExpectedVersion: nodeVersion,
		Kind: operations.ConfigPlan, Candidate: rendered.Candidate, CandidateHash: rendered.Hash[:], PlanRevision: uint64(request.ExpectedRevision),
		TTL: request.TTL, RequestID: request.RequestID, Traceparent: request.Traceparent,
		ActorID: request.ActorID, ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID,
		Action: "config.plan", Reason: request.Reason,
		OcservVersion: ocservVersion, PlanCapabilities: rendered.RequiredCapabilities,
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
	var plan Plan
	var hash []byte
	var storedWarnings []byte
	var result []byte
	var candidateRedacted string
	err := s.pool.QueryRow(ctx, `
		SELECT p.id,p.workspace_id,p.node_id,p.operation_id,p.template_name,p.expected_revision,p.candidate_hash,
		       o.state,p.candidate_redacted,p.warnings,p.expires_at,p.created_at,
		       COALESCE((SELECT r.result FROM agent_command_results r WHERE r.command_id=o.command_id ORDER BY r.created_at DESC LIMIT 1),''::bytea),
		       a.id,COALESCE(a.status,'')
		FROM config_plans p JOIN operations o ON o.id=p.operation_id
		LEFT JOIN LATERAL (SELECT id,status FROM approval_requests WHERE resource_type='config_plan' AND resource_id=p.id AND action='config.apply' ORDER BY created_at DESC LIMIT 1) a ON true
		WHERE p.id=$1`, id).Scan(&plan.ID, &plan.WorkspaceID, &plan.NodeID, &plan.OperationID, &plan.TemplateName, &plan.ExpectedRevision, &hash, &plan.State, &candidateRedacted, &storedWarnings, &plan.ExpiresAt, &plan.CreatedAt, &result, &plan.ApprovalID, &plan.ApprovalStatus)
	if err != nil {
		return Plan{}, err
	}
	plan.CandidateHash = hex.EncodeToString(hash)
	_ = json.Unmarshal(storedWarnings, &plan.Warnings)
	expectedDiff := safeDiff(candidateRedacted)
	plan.Validation = "pending"
	if plan.State == "failed" || plan.State == "rejected" || plan.State == "unknown" || plan.State == "expired" {
		plan.Validation = plan.State
	}
	if len(result) != 0 && plan.State == "succeeded" {
		var validation agentv1.ConfigPlanResult
		if proto.Unmarshal(result, &validation) != nil {
			plan.Validation = "failed"
		} else {
			if bytes.Equal(validation.GetCandidateHash(), hash) && validation.GetDiffRedacted() == expectedDiff && safeValidationWarnings(validation.GetWarnings()) && validation.GetCurrentUnchanged() && validation.GetStagingCleaned() {
				plan.Validation = "valid"
				plan.DiffRedacted = expectedDiff
				plan.Warnings = append(plan.Warnings, validation.GetWarnings()...)
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
	if plan.Validation != "valid" || plan.CurrentHash == "" || !plan.ExpiresAt.After(s.now()) {
		return operations.Operation{}, false, ErrStaleRevision
	}
	var nodeVersion, highestDesiredRevision int64
	var envelopeBytes []byte
	if err := s.pool.QueryRow(ctx, `SELECT n.version,COALESCE(state.desired_revision,0),c.envelope FROM nodes n JOIN config_plans p ON p.node_id=n.id JOIN commands c ON c.operation_id=p.operation_id LEFT JOIN node_config_state state ON state.node_id=n.id WHERE p.id=$1`, request.PlanID).Scan(&nodeVersion, &highestDesiredRevision, &envelopeBytes); err != nil {
		return operations.Operation{}, false, err
	}
	var envelope agentv1.CommandEnvelope
	if proto.Unmarshal(envelopeBytes, &envelope) != nil || envelope.GetConfigPlan() == nil {
		return operations.Operation{}, false, ErrInvalid
	}
	previousHash, err := hex.DecodeString(plan.CurrentHash)
	if err != nil || len(previousHash) != 32 {
		return operations.Operation{}, false, ErrInvalid
	}
	return s.operations.CreateSynthetic(ctx, operations.CreateRequest{
		NodeID: plan.NodeID, IdempotencyKey: request.IdempotencyKey, ExpectedVersion: nodeVersion,
		Kind: operations.ConfigApply, Candidate: envelope.GetConfigPlan().GetCandidate(), CandidateHash: envelope.GetConfigPlan().GetCandidateHash(),
		ExpectedCurrentHash: previousHash, PlanRevision: uint64(plan.ExpectedRevision), DesiredRevision: uint64(highestDesiredRevision) + 1,
		ApplyMetadata: &operations.ConfigApplyMetadata{PlanID: request.PlanID}, ApprovalID: request.ApprovalID,
		TTL: 15 * time.Minute, RequestID: request.RequestID, Traceparent: request.Traceparent,
		ActorID: request.ActorID, ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID,
		Action: "config.apply", Reason: request.Reason,
	})
}

func (s *Service) Resource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id FROM config_plans WHERE id=$1`, id).Scan(&workspaceID, &nodeID)
	return
}

func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

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
