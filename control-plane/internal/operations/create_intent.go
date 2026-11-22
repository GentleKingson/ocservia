package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	configurationstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	tracecontext "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func validateConfigurationIntent(ctx context.Context, tx database.Tx, intentStore operationstore.Store, request CreateRequest, workspaceID uuid.UUID, created time.Time) error {
	switch payload := request.Payload.(type) {
	case ConfigPlanPayload:
		configStore, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		state, err := configStore.State(ctx, request.NodeID)
		if err != nil {
			return fmt.Errorf("read configuration revision: %w", err)
		}
		if state.Revision < 0 || uint64(state.Revision) != payload.PlanRevision {
			return ErrStaleRevision
		}
		if state.OcservVersion != payload.OcservVersion {
			return ErrStaleRevision
		}
		for _, required := range payload.PlanCapabilities {
			approved, err := intentStore.HasCapability(ctx, request.NodeID, required)
			if err != nil {
				return fmt.Errorf("recheck configuration capability: %w", err)
			}
			if !approved {
				return ErrCapabilityMissing
			}
		}
	case ConfigApplyPayload:
		configStore, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		plan, err := configStore.Proof(ctx, payload.ApplyMetadata.PlanID)
		if err != nil {
			return fmt.Errorf("lock configuration plan: %w", err)
		}
		var validation agentv1.ConfigPlanResult
		if plan.WorkspaceID != workspaceID || plan.NodeID != request.NodeID || plan.ExpectedRevision < 0 || uint64(plan.ExpectedRevision) != payload.PlanRevision || !bytes.Equal(plan.CandidateHash, payload.CandidateHash) || plan.ExpiresAt.IsZero() || !plan.ExpiresAt.After(created) || plan.State != "succeeded" || proto.Unmarshal(plan.Result, &validation) != nil || !validation.GetCurrentUnchanged() || !validation.GetStagingCleaned() || !bytes.Equal(validation.GetCandidateHash(), plan.CandidateHash) || !bytes.Equal(validation.GetCurrentHash(), payload.ExpectedCurrentHash) {
			return ErrStaleRevision
		}
		if payload.CompleteCandidate != nil {
			if !proto.Equal(timestamppb.New(plan.ExpiresAt.UTC()), payload.PlanExpiresAt) || !bytes.Equal(validation.GetMaterializedHash(), payload.MaterializedHash) || len(validation.GetWarnings()) != 0 {
				return ErrStaleRevision
			}
		} else if len(validation.GetMaterializedHash()) != 0 {
			return ErrInvalidRequest
		}
		state, err := configStore.State(ctx, request.NodeID)
		if err != nil {
			return fmt.Errorf("read configuration apply fence: %w", err)
		}
		if state.Locked || state.Revision != plan.ExpectedRevision || state.DesiredRevision < state.Revision || payload.DesiredRevision != uint64(state.DesiredRevision)+1 {
			return ErrStaleRevision
		}
		applyActive, err := configStore.HasActiveApply(ctx, request.NodeID)
		if err != nil {
			return fmt.Errorf("check active configuration apply: %w", err)
		}
		if applyActive {
			return ErrConfigApplyActive
		}
	}
	return nil
}

func validateObservedTarget(ctx context.Context, intentStore operationstore.Store, request CreateRequest) error {
	if sessionID, bootID, ok := sessionTarget(request.Payload); ok {
		present, err := intentStore.HasSession(ctx, request.NodeID, sessionID, bootID)
		if err != nil {
			return fmt.Errorf("check observed session: %w", err)
		}
		if !present {
			return ErrTargetNotObserved
		}
	}
	if payload, ok := request.Payload.(IPBanRemovePayload); ok {
		present, err := intentStore.HasIPBan(ctx, request.NodeID, payload.IP)
		if err != nil {
			return fmt.Errorf("check observed IP ban: %w", err)
		}
		if !present {
			return ErrTargetNotObserved
		}
	}
	return nil
}

func insertDomainIntent(ctx context.Context, tx database.Tx, intentStore operationstore.Store, request CreateRequest, workspaceID, operationID uuid.UUID, created, expiry time.Time, fromVersion string) error {
	switch payload := request.Payload.(type) {
	case ConfigPlanPayload:
		warnings, err := json.Marshal(payload.PlanMetadata.Warnings)
		if err != nil {
			return fmt.Errorf("marshal configuration plan warnings: %w", err)
		}
		configStore, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := configStore.InsertPlan(ctx, configurationstore.PendingPlan{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, TemplateName: payload.PlanMetadata.TemplateName, ExpectedRevision: payload.PlanRevision, CandidateHash: payload.CandidateHash, CandidateRedacted: payload.PlanMetadata.CandidateRedacted, Warnings: warnings, ExpiresAt: expiry, CreatedBy: optionalUUID(payload.PlanMetadata.CreatedBy), CreatedAt: created}); err != nil {
			return fmt.Errorf("insert configuration plan: %w", err)
		}
	case ConfigApplyPayload:
		configStore, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := configStore.InsertApply(ctx, configurationstore.PendingApply{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, PlanID: payload.ApplyMetadata.PlanID, ExpectedRevision: payload.PlanRevision, DesiredRevision: payload.DesiredRevision, CandidateHash: payload.CandidateHash, PreviousHash: payload.ExpectedCurrentHash, CreatedAt: created}); err != nil {
			return fmt.Errorf("insert configuration apply: %w", err)
		}
		advanced, err := configStore.AdvanceDesiredRevision(ctx, request.NodeID, payload.DesiredRevision, created)
		if err != nil {
			return fmt.Errorf("advance configuration desired revision: %w", err)
		}
		if !advanced {
			return ErrStaleRevision
		}
	case CertificateCSRPayload:
		dnsNames, err := json.Marshal(payload.DNSNames)
		if err != nil {
			return fmt.Errorf("marshal certificate DNS names: %w", err)
		}
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.InsertCSR(ctx, certificatestore.PendingCSR{ID: payload.CertificateID, WorkspaceID: workspaceID, NodeID: request.NodeID, OperationID: operationID, CommonName: payload.CommonName, DNSNames: dnsNames, KeyBits: payload.KeyBits, CreatedAt: created}); err != nil {
			return fmt.Errorf("insert certificate request: %w", err)
		}
	case CertificateP12Payload:
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.InsertArtifact(ctx, certificatestore.PendingArtifact{ID: payload.ArtifactID, WorkspaceID: workspaceID, NodeID: request.NodeID, CertificateID: payload.CertificateID, CertificateVersion: payload.CertificateVersion, OperationID: operationID, RequesterID: request.ActorIdentityID, TokenSHA256: payload.ArtifactMetadata.TokenSHA256, RequestHash: payload.ArtifactMetadata.RequestHash, ExpiresAt: payload.ArtifactMetadata.ExpiresAt.UTC(), CreatedAt: created}); err != nil {
			return fmt.Errorf("insert certificate artifact operation: %w", err)
		}
	case AgentUpgradePayload:
		if err := intentStore.InsertUpgrade(ctx, operationstore.PendingUpgrade{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, TargetVersion: payload.TargetVersion, PackageSHA256: payload.PackageSHA256, Architecture: payload.Architecture, FromVersion: fromVersion, CreatedAt: created}); err != nil {
			return fmt.Errorf("insert agent upgrade operation: %w", err)
		}

	}
	return nil
}

func (s *Service) lockRolloutObservation(ctx context.Context, intentStore operationstore.Store, request CreateRequest, upgrade AgentUpgradePayload, workspaceID uuid.UUID, nodeStatus string, now time.Time) (*rolloutNodeObservation, error) {
	// Serialize generated operation intent, including idempotent replay,
	// with rollout pause/resume. A cleared claim or non-running rollout
	// must not create or recover an operation from an older memory claim.
	claim, err := intentStore.LockRolloutClaim(ctx, upgrade.RolloutID, workspaceID, request.NodeID)
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrRolloutState
	}
	if err != nil {
		return nil, fmt.Errorf("lock rollout dispatch claim: %w", err)
	}
	if claim.State != RolloutStateRunning || claim.NodeState != RolloutNodePending || claim.DispatchLease == nil || !claim.DispatchLease.After(now) {
		return nil, ErrRolloutState
	}
	if s.releaseCatalog == nil {
		return nil, ErrRolloutState
	}
	observed, err := intentStore.LockAgentObservation(ctx, request.NodeID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil, ErrStaleRevision
		}
		return nil, fmt.Errorf("lock rollout node observation: %w", err)
	}
	observation := rolloutNodeObservation{NodeID: request.NodeID, Status: nodeStatus, Architecture: observed.Architecture, AgentVersion: observed.AgentVersion, LastHeartbeatAt: observed.LastHeartbeatAt}
	observation.CapabilityOK, err = intentStore.LockUpgradeCapability(ctx, request.NodeID)
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrCapabilityMissing
	}
	if err != nil {
		return nil, fmt.Errorf("lock rollout node capability: %w", err)
	}
	observation.UpgradeActive, err = intentStore.HasActiveUpgrade(ctx, request.NodeID)
	if err != nil {
		return nil, fmt.Errorf("recheck active rollout upgrade: %w", err)
	}
	reason, digest, eligible := s.rolloutNodeEligibility(now, observation, upgrade.TargetVersion)
	if !eligible {
		switch reason {
		case "not_trusted", "offline":
			return nil, ErrNodeUnavailable
		case "missing_capability":
			return nil, ErrCapabilityMissing
		case "upgrade_in_progress":
			return nil, ErrUpgradeActive
		default:
			return nil, ErrStaleRevision
		}
	}
	if observation.Architecture != upgrade.Architecture || !bytes.Equal(digest[:], upgrade.PackageSHA256) {
		return nil, ErrStaleRevision
	}
	rollout, err := intentStore.Rollout(ctx, upgrade.RolloutID, false)
	if err != nil {
		return nil, err
	}
	if rollout.WorkspaceID != workspaceID || rollout.CreatedBy != request.ActorIdentityID || rollout.ActorSession != request.ActorSessionID || rollout.TargetVersion != upgrade.TargetVersion || !bytes.Equal(rollout.RequestHash, upgrade.RolloutRequestHash) {
		return nil, ErrRolloutState
	}

	return &observation, nil

}

func appendIntentAudit(ctx context.Context, tx database.Tx, request CreateRequest, workspaceID, operationID, commandID, auditID uuid.UUID, fromVersion string, now time.Time) error {
	actorID, action, reason := normalizedAuditIntent(request)
	var auditSummary json.RawMessage
	switch payload := request.Payload.(type) {
	case ConfigPlanPayload:
		auditSummary, _ = json.Marshal(map[string]any{"candidate_hash": fmt.Sprintf("%x", payload.CandidateHash), "expected_revision": payload.PlanRevision})
	case ConfigApplyPayload:
		auditSummary, _ = json.Marshal(map[string]any{"plan_id": payload.ApplyMetadata.PlanID, "candidate_hash": fmt.Sprintf("%x", payload.CandidateHash), "previous_hash": fmt.Sprintf("%x", payload.ExpectedCurrentHash), "desired_revision": payload.DesiredRevision})
	case AgentUpgradePayload:
		summary := map[string]any{"from_version": fromVersion, "target_version": payload.TargetVersion, "architecture": payload.Architecture, "package_sha256": fmt.Sprintf("%x", payload.PackageSHA256)}
		if payload.RolloutID != uuid.Nil {
			summary["rollout_id"] = payload.RolloutID
		}
		auditSummary, _ = json.Marshal(summary)

	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "user", ActorID: actorID, SessionID: optionalUUID(request.ActorSessionID), Action: action, ResourceType: "operation", ResourceID: operationID, NodeID: &request.NodeID, CommandID: &commandID, RequestID: request.RequestID, TraceID: tracecontext.TraceID(request.Traceparent), Reason: reason, AfterSummary: auditSummary, At: now}); err != nil {
		return fmt.Errorf("append operation audit intent: %w", err)
	}
	return nil
}

func bindCommandCapability(ctx context.Context, intentStore operationstore.Store, request *CreateRequest) error {
	if capability := requestCapability(*request); capability != "" {
		approved, err := intentStore.HasCapability(ctx, request.NodeID, capability)
		if err != nil {
			return fmt.Errorf("check operation capability: %w", err)
		}
		if !approved && request.Payload.kind() == AgentUpgrade {
			capability = "ocserv.agent.upgrade.v1"
			approved, err = intentStore.HasCapability(ctx, request.NodeID, capability)
			if err != nil {
				return fmt.Errorf("check operation capability: %w", err)
			}
		}
		if !approved {
			return ErrCapabilityMissing
		}
		if payload, ok := request.Payload.(AgentUpgradePayload); ok {
			payload.upgradeCapability = capability
			request.Payload = payload
		}
	}
	return nil
}
