package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	configurationstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/tracecontext"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrInvalidRequest      = errors.New("invalid operation request")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with different input")
	ErrStaleRevision       = errors.New("resource revision is stale")
	ErrNodeUnavailable     = errors.New("node is unavailable")
	ErrConfigApplyActive   = errors.New("a configuration apply is already active for this node")
	ErrUpgradeActive       = errors.New("an agent upgrade is already active for this node")
	ErrCapabilityMissing   = errors.New("node capability is unavailable")
	ErrTargetNotObserved   = errors.New("target is not present in observed state")
	ErrBacklogExceeded     = commandlimit.ErrBacklogExceeded
)

const (
	// A transport-accepted attempt without durable result evidence becomes an
	// Unknown outcome after this interval. Recovery observes the Agent journal;
	// it never replays the original effect blindly.
	// Leave recovery headroom below the 30-second end-to-end command target.
	// Reconciliation is observation-only, so advancing it cannot replay an
	// effect whose immediate result was lost.
	commandResultResponseTimeout = 20 * time.Second
	reconciliationAttemptLimit   = 64
	reconciliationBatchLimit     = 64
	// The configured active-command ceiling is 500. Decode that full bounded
	// candidate set before applying the reconcile-only batch limit so unrelated
	// Unknown outcomes cannot permanently hide eligible continuation work.
	reconciliationCandidateScanLimit = 500
)

type Kind string

const (
	SyntheticNoop     Kind = "noop"
	SyntheticEcho     Kind = "echo"
	SessionDisconnect Kind = "session_disconnect"
	SessionTerminate  Kind = "session_terminate"
	IPBanRemove       Kind = "ip_ban_remove"
	ServiceReload     Kind = "service_reload"
	ConfigPlan        Kind = "config_plan"
	ConfigApply       Kind = "config_apply"
	CertificateCSR    Kind = "certificate_csr"
	CertificateP12    Kind = "certificate_p12"
	CertificateRevoke Kind = "certificate_revoke"
	AgentUpgrade      Kind = "agent_upgrade"
)

// CreateRequest carries shared admission and audit context. Payload owns the
// target and effect fields; its concrete type determines the operation kind.
type CreateRequest struct {
	NodeID           uuid.UUID
	IdempotencyKey   string
	ExpectedVersion  int64
	Payload          Payload
	ActorID          string
	ActorIdentityID  uuid.UUID
	ActorSessionID   uuid.UUID
	Action           string
	Reason           string
	SupersedePending bool
	TTL              time.Duration
	RequestID        string
	Traceparent      string
}

// ConfigPlanMetadata is written atomically with the remote validation intent.
type ConfigPlanMetadata struct {
	TemplateName      string
	CandidateRedacted string
	Warnings          []string
	CreatedBy         uuid.UUID
}

// ConfigApplyMetadata binds an approved immutable plan to its dispatch intent.
type ConfigApplyMetadata struct {
	PlanID uuid.UUID
}

type ArtifactMetadata struct {
	TokenSHA256 []byte
	RequestHash []byte
	ExpiresAt   time.Time
}

type Operation = operationstore.Operation

type Event = operationstore.Event

type Dispatch = operationstore.Dispatch

type QueueMetrics = operationstore.QueueMetrics

// defaultAgentUpgradeReconcileTimeout bounds the whole scheduled-upgrade
// lifecycle: after it elapses without conclusive evidence the reconciliation
// loop must move the operation to a terminal unknown instead of waiting
// forever.
const defaultAgentUpgradeReconcileTimeout = 30 * time.Minute

type Service struct {
	backend                   database.Backend
	now                       func() time.Time
	commandLimit              int
	signer                    *commandauth.Signer
	agentUpgradeReconcileTime time.Duration
	releaseCatalog            *releasecatalog.Catalog
}

// NewBackend supplies the common operation and background Worker lifecycle.
func NewBackend(backend database.Backend, commandLimit int, signer *commandauth.Signer) *Service {
	return &Service{backend: backend, now: func() time.Time { return time.Now().UTC() }, commandLimit: commandLimit, signer: signer, agentUpgradeReconcileTime: defaultAgentUpgradeReconcileTimeout}
}

// SetAgentUpgradeReconcileTimeout installs the bounded single-node upgrade
// reconciliation window. Values outside 1m..24h are refused.
func (s *Service) SetAgentUpgradeReconcileTimeout(value time.Duration) error {
	if value < time.Minute || value > 24*time.Hour {
		return errors.New("agent upgrade reconcile timeout must be between 1m and 24h")
	}
	s.agentUpgradeReconcileTime = value
	return nil
}

// EnableReleaseCatalog installs the operator-provisioned trusted release
// catalog used for single-node upgrade preparation and rollout-dispatched
// upgrades. Without it, release resolution fails closed.
func (s *Service) EnableReleaseCatalog(catalog *releasecatalog.Catalog) {
	s.releaseCatalog = catalog
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Operation, bool, error) {
	if err := validateCreate(request); err != nil {
		return Operation{}, false, err
	}
	now := s.now()
	created, err := value.FromTime(now)
	if err != nil {
		return Operation{}, false, err
	}
	hash := requestHash(request)
	tx, err := s.backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return Operation{}, false, fmt.Errorf("begin operation transaction: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	commonTx := tx
	intentStore, err := operationstore.FromTransaction(commonTx)
	if err != nil {
		return Operation{}, false, err
	}

	var rolloutObservation *rolloutNodeObservation
	upgrade, _ := request.Payload.(AgentUpgradePayload)
	var fromVersion string
	node, err := intentStore.LockNode(ctx, request.NodeID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return Operation{}, false, ErrNodeUnavailable
		}
		return Operation{}, false, fmt.Errorf("lock operation node: %w", err)
	}
	workspaceID, nodeVersion, authorizationRevision, nodeStatus := node.WorkspaceID, node.Version, node.AuthorizationRevision, node.Status
	if nodeStatus != "active" && nodeStatus != "offline" {
		return Operation{}, false, ErrNodeUnavailable
	}
	if upgrade.RolloutID != uuid.Nil {
		// Serialize generated operation intent, including idempotent replay,
		// with rollout pause/resume. A cleared claim or non-running rollout
		// must not create or recover an operation from an older memory claim.
		claim, err := intentStore.LockRolloutClaim(ctx, upgrade.RolloutID, workspaceID, request.NodeID)
		if errors.Is(err, database.ErrNotFound) {
			return Operation{}, false, ErrRolloutState
		}
		if err != nil {
			return Operation{}, false, fmt.Errorf("lock rollout dispatch claim: %w", err)
		}
		if claim.State != RolloutStateRunning || claim.NodeState != RolloutNodePending || !claim.DispatchLease.Valid || claim.DispatchLease.Micros <= created.Micros {
			return Operation{}, false, ErrRolloutState
		}
		if s.releaseCatalog == nil {
			return Operation{}, false, ErrRolloutState
		}
		observed, err := intentStore.LockAgentObservation(ctx, request.NodeID)
		if err != nil {
			if errors.Is(err, database.ErrNotFound) {
				return Operation{}, false, ErrStaleRevision
			}
			return Operation{}, false, fmt.Errorf("lock rollout node observation: %w", err)
		}
		observation := rolloutNodeObservation{NodeID: request.NodeID, Status: nodeStatus, Architecture: observed.Architecture, AgentVersion: observed.AgentVersion, LastHeartbeatAt: observed.LastHeartbeatAt}
		observation.CapabilityOK, err = intentStore.LockUpgradeCapability(ctx, request.NodeID)
		if errors.Is(err, database.ErrNotFound) {
			return Operation{}, false, ErrCapabilityMissing
		}
		if err != nil {
			return Operation{}, false, fmt.Errorf("lock rollout node capability: %w", err)
		}
		observation.UpgradeActive, err = intentStore.HasActiveUpgrade(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("recheck active rollout upgrade: %w", err)
		}
		reason, digest, eligible := s.rolloutNodeEligibility(now, observation, upgrade.TargetVersion)
		if !eligible {
			switch reason {
			case "not_trusted", "offline":
				return Operation{}, false, ErrNodeUnavailable
			case "missing_capability":
				return Operation{}, false, ErrCapabilityMissing
			case "upgrade_in_progress":
				return Operation{}, false, ErrUpgradeActive
			default:
				return Operation{}, false, ErrStaleRevision
			}
		}
		if observation.Architecture != upgrade.Architecture || !bytes.Equal(digest[:], upgrade.PackageSHA256) {
			return Operation{}, false, ErrStaleRevision
		}
		rollout, err := intentStore.Rollout(ctx, upgrade.RolloutID, false)
		if err != nil {
			return Operation{}, false, err
		}
		if rollout.WorkspaceID != workspaceID || rollout.CreatedBy != request.ActorIdentityID || rollout.ActorSession != request.ActorSessionID || rollout.TargetVersion != upgrade.TargetVersion || !bytes.Equal(rollout.RequestHash, upgrade.RolloutRequestHash) {
			return Operation{}, false, ErrRolloutState
		}

		rolloutObservation = &observation
	}
	if upgrade.RolloutID == uuid.Nil {
		switch request.Payload.(type) {
		case ConfigApplyPayload, CertificateP12Payload, CertificateRevokePayload, ServiceReloadPayload, AgentUpgradePayload:
			if err := auth.AuthorizeMutationTx(ctx, commonTx, request.ActorIdentityID, request.ActorSessionID, workspaceID); err != nil {
				return Operation{}, false, err
			}
		}
	}
	if existing, same, err := findIdempotent(ctx, commonTx, workspaceID, request.IdempotencyKey, hash[:]); err != nil {
		return Operation{}, false, err
	} else if existing.ID != "" {
		if !same {
			return Operation{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Operation{}, false, fmt.Errorf("commit idempotent lookup: %w", err)
		}
		return existing, true, nil
	}
	if nodeVersion != request.ExpectedVersion {
		return Operation{}, false, ErrStaleRevision
	}
	if request.Payload.kind() != SyntheticNoop && request.Payload.kind() != SyntheticEcho {
		attestationReady, err := intentStore.AttestationReady(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("recheck privd result attestation: %w", err)
		}
		if !attestationReady {
			return Operation{}, false, ErrCapabilityMissing
		}
	}
	switch payload := request.Payload.(type) {
	case ConfigPlanPayload:
		configStore, err := configurationstore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		state, err := configStore.State(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("read configuration revision: %w", err)
		}
		if state.Revision < 0 || uint64(state.Revision) != payload.PlanRevision {
			return Operation{}, false, ErrStaleRevision
		}
		if state.OcservVersion != payload.OcservVersion {
			return Operation{}, false, ErrStaleRevision
		}
		for _, required := range payload.PlanCapabilities {
			approved, err := intentStore.HasCapability(ctx, request.NodeID, required)
			if err != nil {
				return Operation{}, false, fmt.Errorf("recheck configuration capability: %w", err)
			}
			if !approved {
				return Operation{}, false, ErrCapabilityMissing
			}
		}
	case ConfigApplyPayload:
		configStore, err := configurationstore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		plan, err := configStore.Proof(ctx, payload.ApplyMetadata.PlanID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("lock configuration plan: %w", err)
		}
		var validation agentv1.ConfigPlanResult
		if plan.WorkspaceID != workspaceID || plan.NodeID != request.NodeID || plan.ExpectedRevision < 0 || uint64(plan.ExpectedRevision) != payload.PlanRevision || !bytes.Equal(plan.CandidateHash, payload.CandidateHash) || !plan.ExpiresAt.Valid || plan.ExpiresAt.Micros <= created.Micros || plan.State != "succeeded" || proto.Unmarshal(plan.Result, &validation) != nil || !validation.GetCurrentUnchanged() || !validation.GetStagingCleaned() || !bytes.Equal(validation.GetCandidateHash(), plan.CandidateHash) || !bytes.Equal(validation.GetCurrentHash(), payload.ExpectedCurrentHash) {
			return Operation{}, false, ErrStaleRevision
		}
		if payload.CompleteCandidate != nil {
			expires, expiryErr := plan.ExpiresAt.Time()
			if expiryErr != nil || !proto.Equal(timestamppb.New(expires), payload.PlanExpiresAt) || !bytes.Equal(validation.GetMaterializedHash(), payload.MaterializedHash) || len(validation.GetWarnings()) != 0 {
				return Operation{}, false, ErrStaleRevision
			}
		} else if len(validation.GetMaterializedHash()) != 0 {
			return Operation{}, false, ErrInvalidRequest
		}
		state, err := configStore.State(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("read configuration apply fence: %w", err)
		}
		if state.Locked || state.Revision != plan.ExpectedRevision || state.DesiredRevision < state.Revision || payload.DesiredRevision != uint64(state.DesiredRevision)+1 {
			return Operation{}, false, ErrStaleRevision
		}
		applyActive, err := configStore.HasActiveApply(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check active configuration apply: %w", err)
		}
		if applyActive {
			return Operation{}, false, ErrConfigApplyActive
		}
	}
	if capability := requestCapability(request); capability != "" {
		approved, err := intentStore.HasCapability(ctx, request.NodeID, capability)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check operation capability: %w", err)
		}
		if !approved && request.Payload.kind() == AgentUpgrade {
			capability = "ocserv.agent.upgrade.v1"
			approved, err = intentStore.HasCapability(ctx, request.NodeID, capability)
			if err != nil {
				return Operation{}, false, fmt.Errorf("check operation capability: %w", err)
			}
		}
		if !approved {
			return Operation{}, false, ErrCapabilityMissing
		}
		if payload, ok := request.Payload.(AgentUpgradePayload); ok {
			payload.upgradeCapability = capability
			request.Payload = payload
		}
	}
	if sessionID, bootID, ok := sessionTarget(request.Payload); ok {
		present, err := intentStore.HasSession(ctx, request.NodeID, sessionID, bootID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check observed session: %w", err)
		}
		if !present {
			return Operation{}, false, ErrTargetNotObserved
		}
	}
	if payload, ok := request.Payload.(IPBanRemovePayload); ok {
		present, err := intentStore.HasIPBan(ctx, request.NodeID, payload.IP)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check observed IP ban: %w", err)
		}
		if !present {
			return Operation{}, false, ErrTargetNotObserved
		}
	}
	if _, ok := request.Payload.(AgentUpgradePayload); ok {
		var observedVersion string
		if rolloutObservation != nil {
			observedVersion = rolloutObservation.AgentVersion
		} else {
			observed, err := intentStore.LockAgentObservation(ctx, request.NodeID)
			if errors.Is(err, database.ErrNotFound) {
				observedVersion = ""
			} else if err != nil {
				return Operation{}, false, fmt.Errorf("read observed agent version: %w", err)
			} else {
				observedVersion = observed.AgentVersion
			}
		}
		fromVersion = observedVersion
		upgradeActive, err := intentStore.HasActiveUpgrade(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check active agent upgrade: %w", err)
		}
		if upgradeActive {
			return Operation{}, false, ErrUpgradeActive
		}
	}

	operationID, commandID, outboxID, auditID, eventID, err := newIDs(5)
	if err != nil {
		return Operation{}, false, err
	}
	expiresAt := now.Add(request.TTL)
	expiry, err := value.FromTime(expiresAt)
	if err != nil {
		return Operation{}, false, err
	}
	envelope, payloadType, err := marshalEnvelope(request, operationID, commandID, authorizationRevision, now, expiresAt, s.signer)
	if err != nil {
		return Operation{}, false, err
	}
	if request.SupersedePending {
		if err := intentStore.SupersedePending(ctx, request.NodeID, payloadType, created); err != nil {
			return Operation{}, false, err
		}
	}
	if err := commandlimit.ReserveBacklog(ctx, commonTx, workspaceID, request.NodeID); err != nil {
		return Operation{}, false, err
	}
	if err := intentStore.InsertIntent(ctx, operationstore.QueuedIntent{
		ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, CommandID: commandID,
		RequestID: request.RequestID, TraceID: tracecontext.TraceID(request.Traceparent), IdempotencyKey: request.IdempotencyKey,
		RequestHash: hash[:], ExpiresAt: expiry, CreatedAt: created,
	}); err != nil {
		return Operation{}, false, fmt.Errorf("insert operation intent: %w", err)
	}
	switch payload := request.Payload.(type) {
	case ConfigPlanPayload:
		warnings, err := json.Marshal(payload.PlanMetadata.Warnings)
		if err != nil {
			return Operation{}, false, fmt.Errorf("marshal configuration plan warnings: %w", err)
		}
		logicalWarnings, err := value.ParseJSONB(warnings)
		if err != nil {
			return Operation{}, false, err
		}
		configStore, err := configurationstore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		if err := configStore.InsertPlan(ctx, configurationstore.PendingPlan{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, TemplateName: payload.PlanMetadata.TemplateName, ExpectedRevision: payload.PlanRevision, CandidateHash: payload.CandidateHash, CandidateRedacted: payload.PlanMetadata.CandidateRedacted, Warnings: logicalWarnings, ExpiresAt: expiry, CreatedBy: optionalUUID(payload.PlanMetadata.CreatedBy), CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert configuration plan: %w", err)
		}
	case ConfigApplyPayload:
		configStore, err := configurationstore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		if err := configStore.InsertApply(ctx, configurationstore.PendingApply{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, PlanID: payload.ApplyMetadata.PlanID, ExpectedRevision: payload.PlanRevision, DesiredRevision: payload.DesiredRevision, CandidateHash: payload.CandidateHash, PreviousHash: payload.ExpectedCurrentHash, CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert configuration apply: %w", err)
		}
		advanced, err := configStore.AdvanceDesiredRevision(ctx, request.NodeID, payload.DesiredRevision, created)
		if err != nil {
			return Operation{}, false, fmt.Errorf("advance configuration desired revision: %w", err)
		}
		if !advanced {
			return Operation{}, false, ErrStaleRevision
		}
	case CertificateCSRPayload:
		dnsNames, err := json.Marshal(payload.DNSNames)
		if err != nil {
			return Operation{}, false, fmt.Errorf("marshal certificate DNS names: %w", err)
		}
		names, err := value.ParseJSONB(dnsNames)
		if err != nil {
			return Operation{}, false, err
		}
		store, err := certificatestore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		if err := store.InsertCSR(ctx, certificatestore.PendingCSR{ID: payload.CertificateID, WorkspaceID: workspaceID, NodeID: request.NodeID, OperationID: operationID, CommonName: payload.CommonName, DNSNames: names, KeyBits: payload.KeyBits, CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert certificate request: %w", err)
		}
	case CertificateP12Payload:
		store, err := certificatestore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		artifactExpiry, err := value.FromTime(payload.ArtifactMetadata.ExpiresAt)
		if err != nil {
			return Operation{}, false, err
		}
		if err := store.InsertArtifact(ctx, certificatestore.PendingArtifact{ID: payload.ArtifactID, WorkspaceID: workspaceID, NodeID: request.NodeID, CertificateID: payload.CertificateID, CertificateVersion: payload.CertificateVersion, OperationID: operationID, RequesterID: request.ActorIdentityID, TokenSHA256: payload.ArtifactMetadata.TokenSHA256, RequestHash: payload.ArtifactMetadata.RequestHash, ExpiresAt: artifactExpiry, CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert certificate artifact operation: %w", err)
		}
	case AgentUpgradePayload:
		if err := intentStore.InsertUpgrade(ctx, operationstore.PendingUpgrade{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, TargetVersion: payload.TargetVersion, PackageSHA256: payload.PackageSHA256, Architecture: payload.Architecture, FromVersion: fromVersion, CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert agent upgrade operation: %w", err)
		}

	}
	availableAt := created
	if payload, ok := request.Payload.(CertificateRevokePayload); ok && payload.HoldDispatch {
		availableAt = expiry
	}
	if err := intentStore.EnqueueCommand(ctx, operationstore.QueuedCommand{
		ID: commandID, OperationID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID,
		OutboxID: outboxID, EventID: eventID, PayloadType: payloadType, Envelope: envelope,
		IdempotencyKey: request.IdempotencyKey, ExpectedVersion: request.ExpectedVersion, Traceparent: request.Traceparent,
		ExpiresAt: expiry, CreatedAt: created, AvailableAt: availableAt,
	}); err != nil {
		return Operation{}, false, err
	}
	actorID, action, reason := normalizedAuditIntent(request)
	var auditSummary json.RawMessage
	switch payload := request.Payload.(type) {
	case ConfigPlanPayload:
		auditSummary, _ = json.Marshal(map[string]any{"candidate_hash": fmt.Sprintf("%x", payload.CandidateHash), "expected_revision": payload.PlanRevision})
	case ConfigApplyPayload:
		auditSummary, _ = json.Marshal(map[string]any{"plan_id": payload.ApplyMetadata.PlanID, "candidate_hash": fmt.Sprintf("%x", payload.CandidateHash), "previous_hash": fmt.Sprintf("%x", payload.ExpectedCurrentHash), "desired_revision": payload.DesiredRevision})
	case AgentUpgradePayload:
		summary := map[string]any{"from_version": fromVersion, "target_version": payload.TargetVersion, "architecture": payload.Architecture, "package_sha256": fmt.Sprintf("%x", payload.PackageSHA256)}
		if upgrade.RolloutID != uuid.Nil {
			summary["rollout_id"] = upgrade.RolloutID
		}
		auditSummary, _ = json.Marshal(summary)

	}
	if err := audit.AppendChainTx(ctx, commonTx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "user", ActorID: actorID, SessionID: optionalUUID(request.ActorSessionID), Action: action, ResourceType: "operation", ResourceID: operationID, NodeID: &request.NodeID, CommandID: &commandID, RequestID: request.RequestID, TraceID: tracecontext.TraceID(request.Traceparent), Reason: reason, AfterSummary: auditSummary, At: now}); err != nil {
		return Operation{}, false, fmt.Errorf("append operation audit intent: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		// The commit acknowledgement may have been lost. Resolve only this
		// operation's immutable idempotency record; never create another intent.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var existing Operation
		confirmed := database.Within(check, s.backend, database.ReadCommitted, func(read database.Tx) error {
			var same bool
			var err error
			existing, same, err = findIdempotent(check, read, workspaceID, request.IdempotencyKey, hash[:])
			if err == nil && (!same || existing.ID != operationID.String()) {
				return database.ErrNotFound
			}
			return err
		})
		if confirmed == nil {
			return existing, false, nil
		}
		return Operation{}, false, fmt.Errorf("commit operation transaction: %w", err)
	}
	nodeText, commandText := request.NodeID.String(), commandID.String()
	operation := Operation{ID: operationID.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: created, UpdatedAt: created, ExpiresAt: &expiry}
	if _, ok := request.Payload.(ConfigApplyPayload); ok {
		operation.ConfigApplyState = "queued"
	}
	if payload, ok := request.Payload.(AgentUpgradePayload); ok {
		operation.AgentUpgradeState = "queued"
		operation.AgentUpgradeTarget = payload.TargetVersion
	}
	return operation, false, nil
}

func (s *Service) ListEvents(ctx context.Context, operationID, after uuid.UUID, limit int) ([]Event, error) {
	if limit < 1 || limit > 200 {
		return nil, ErrInvalidRequest
	}
	var events []Event
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		events, err = store.Events(ctx, operationID, after, limit)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list operation events: %w", err)
	}
	return events, nil
}

func (s *Service) EventSequence(ctx context.Context, operationID, eventID uuid.UUID) (int64, bool, error) {
	if operationID == uuid.Nil || eventID == uuid.Nil {
		return 0, false, nil
	}
	var sequence int64
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		sequence, err = store.EventSequence(ctx, operationID, eventID)
		return err
	})
	if errors.Is(err, database.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve operation event cursor: %w", err)
	}
	return sequence, true, nil
}

func (s *Service) Expire(ctx context.Context) error {
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.ExpireQueued(ctx)
	})
}

func requestHash(r CreateRequest) [32]byte {
	actorID, action, reason := normalizedAuditIntent(r)
	// Request and trace IDs identify an attempt, not the mutation intent. Every
	// field that selects the target, effect, authorization action, actor, audit
	// reason, revision, or delivery behavior is deliberately bound here.
	intent := struct {
		CompleteProfile      bool                   `json:"complete_profile,omitempty"`
		MaterializedHash     string                 `json:"materialized_hash,omitempty"`
		PlanExpiresAt        *timestamppb.Timestamp `json:"plan_expires_at,omitempty"`
		NodeID               uuid.UUID              `json:"node_id"`
		Kind                 Kind                   `json:"kind"`
		Message              string                 `json:"message"`
		SessionID            string                 `json:"session_id"`
		BootID               string                 `json:"boot_id"`
		IP                   string                 `json:"ip"`
		ExpectedVersion      int64                  `json:"expected_version"`
		SupersedePending     bool                   `json:"supersede_pending"`
		HoldDispatch         bool                   `json:"hold_dispatch,omitempty"`
		TTLSeconds           int64                  `json:"ttl_seconds"`
		ActorID              string                 `json:"actor_id"`
		Action               string                 `json:"action"`
		Reason               string                 `json:"reason"`
		ActorSessionID       uuid.UUID              `json:"actor_session_id"`
		ActorIdentityID      uuid.UUID              `json:"actor_identity_id"`
		CandidateHash        string                 `json:"candidate_hash"`
		ExpectedCurrentHash  string                 `json:"expected_current_hash"`
		DesiredRevision      uint64                 `json:"desired_revision"`
		PlanID               uuid.UUID              `json:"plan_id"`
		PlanRevision         uint64                 `json:"plan_revision"`
		PlanTemplate         string                 `json:"plan_template"`
		OcservVersion        string                 `json:"ocserv_version"`
		PlanCapabilities     []string               `json:"plan_capabilities"`
		CertificateID        uuid.UUID              `json:"certificate_id"`
		CommonName           string                 `json:"common_name"`
		DNSNames             []string               `json:"dns_names"`
		KeyBits              uint32                 `json:"key_bits"`
		ArtifactID           uuid.UUID              `json:"artifact_id"`
		CertificateChainHash string                 `json:"certificate_chain_hash"`
		SealedPasswordHash   string                 `json:"sealed_password_hash"`
		SecretKeyID          string                 `json:"secret_key_id"`
		SecretVersion        int32                  `json:"secret_version"`
		SecretPurpose        int32                  `json:"secret_purpose"`
		CertificateVersion   uint64                 `json:"certificate_version"`
		ArtifactTokenHash    string                 `json:"artifact_token_hash"`
		ArtifactRequestHash  string                 `json:"artifact_request_hash"`
		ArtifactExpiresAt    string                 `json:"artifact_expires_at"`
		RevocationReason     string                 `json:"revocation_reason"`
		TargetVersion        string                 `json:"target_version"`
		PackageSHA256        string                 `json:"package_sha256"`
		Architecture         string                 `json:"architecture"`
	}{NodeID: r.NodeID, Kind: r.Payload.kind(), ExpectedVersion: r.ExpectedVersion,
		SupersedePending: r.SupersedePending, TTLSeconds: int64(r.TTL / time.Second),
		ActorID: actorID, Action: action, Reason: reason, ActorSessionID: r.ActorSessionID,
		ActorIdentityID: r.ActorIdentityID}
	// Keep the historical JSON shape, including zero fields, nil lists and the
	// generated CSR ID exclusion. Changing this projection invalidates receipts.
	switch p := r.Payload.(type) {
	case SyntheticEchoPayload:
		intent.Message = p.Message
	case SessionDisconnectPayload:
		intent.SessionID, intent.BootID = p.SessionID, p.BootID
	case SessionTerminatePayload:
		intent.SessionID, intent.BootID = p.SessionID, p.BootID
	case IPBanRemovePayload:
		intent.IP = p.IP
	case ConfigPlanPayload:
		intent.CompleteProfile = p.CompleteCandidate != nil
		intent.CandidateHash, intent.PlanRevision = fmt.Sprintf("%x", p.CandidateHash), p.PlanRevision
		intent.PlanTemplate, intent.OcservVersion, intent.PlanCapabilities = planTemplate(p.PlanMetadata), p.OcservVersion, p.PlanCapabilities
	case ConfigApplyPayload:
		intent.CompleteProfile, intent.MaterializedHash, intent.PlanExpiresAt = p.CompleteCandidate != nil, hex.EncodeToString(p.MaterializedHash), p.PlanExpiresAt
		intent.CandidateHash, intent.ExpectedCurrentHash = fmt.Sprintf("%x", p.CandidateHash), fmt.Sprintf("%x", p.ExpectedCurrentHash)
		intent.DesiredRevision, intent.PlanRevision, intent.PlanID = p.DesiredRevision, p.PlanRevision, applyPlanID(p.ApplyMetadata)
	case CertificateCSRPayload:
		intent.CommonName, intent.DNSNames, intent.KeyBits = p.CommonName, p.DNSNames, p.KeyBits
	case CertificateP12Payload:
		intent.CertificateID, intent.CertificateVersion, intent.ArtifactID = p.CertificateID, p.CertificateVersion, p.ArtifactID
		intent.CertificateChainHash, intent.SealedPasswordHash = hashBytes(p.CertificateChain), sealedPasswordHash(p.SealedPassword)
		intent.SecretKeyID, intent.SecretVersion, intent.SecretPurpose = sealedPasswordKeyID(p.SealedPassword), sealedPasswordVersion(p.SealedPassword), sealedPasswordPurpose(p.SealedPassword)
		intent.ArtifactTokenHash, intent.ArtifactRequestHash, intent.ArtifactExpiresAt = artifactTokenHash(p.ArtifactMetadata), artifactRequestHash(p.ArtifactMetadata), artifactExpiry(p.ArtifactMetadata)
	case CertificateRevokePayload:
		intent.CertificateID, intent.CertificateVersion, intent.RevocationReason, intent.HoldDispatch = p.CertificateID, p.CertificateVersion, p.RevocationReason, p.HoldDispatch
	case AgentUpgradePayload:
		intent.TargetVersion, intent.PackageSHA256, intent.Architecture = p.TargetVersion, fmt.Sprintf("%x", p.PackageSHA256), p.Architecture
	}

	encoded, err := json.Marshal(intent)
	if err != nil {
		panic("marshal fixed idempotency intent: " + err.Error())
	}
	return sha256.Sum256(encoded)
}

func hashBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validSealedP12(secret *agentv1.SealedSecretV1) bool {
	return secret != nil && secret.GetVersion() == agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1 && secret.GetPurpose() == agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD && strings.TrimSpace(secret.GetKeyId()) != "" && len(secret.GetKeyId()) <= 128 && len(secret.GetCiphertext()) >= 32 && len(secret.GetCiphertext()) <= 16*1024
}
func sealedPasswordHash(secret *agentv1.SealedSecretV1) string {
	if secret == nil {
		return ""
	}
	return hashBytes(secret.GetCiphertext())
}
func sealedPasswordKeyID(secret *agentv1.SealedSecretV1) string {
	if secret == nil {
		return ""
	}
	return secret.GetKeyId()
}
func sealedPasswordVersion(secret *agentv1.SealedSecretV1) int32 {
	if secret == nil {
		return 0
	}
	return int32(secret.GetVersion())
}
func sealedPasswordPurpose(secret *agentv1.SealedSecretV1) int32 {
	if secret == nil {
		return 0
	}
	return int32(secret.GetPurpose())
}

func artifactTokenHash(metadata *ArtifactMetadata) string {
	if metadata == nil {
		return ""
	}
	return hex.EncodeToString(metadata.TokenSHA256)
}

func artifactRequestHash(metadata *ArtifactMetadata) string {
	if metadata == nil {
		return ""
	}
	return hex.EncodeToString(metadata.RequestHash)
}

func artifactExpiry(metadata *ArtifactMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.ExpiresAt.UTC().Format(time.RFC3339Nano)
}

func planTemplate(metadata *ConfigPlanMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.TemplateName
}

func applyPlanID(metadata *ConfigApplyMetadata) uuid.UUID {
	if metadata == nil {
		return uuid.Nil
	}
	return metadata.PlanID
}

func normalizedAuditIntent(r CreateRequest) (actorID, action, reason string) {
	actorID, action, reason = r.ActorID, r.Action, r.Reason
	if actorID == "" {
		actorID = "developer"
	}
	if action == "" {
		action = "operation.create"
	}
	if reason == "" {
		reason = "side-effect-free delivery validation"
	}
	return actorID, action, reason
}

func marshalEnvelope(r CreateRequest, operationID, commandID uuid.UUID, authorizationRevision uint64, now, expires time.Time, signer *commandauth.Signer) ([]byte, string, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, "", err
	}
	actorID, action, reason := normalizedAuditIntent(r)
	envelope := &agentv1.CommandEnvelope{ProtocolVersion: commandauth.ProtocolVersion, MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: r.NodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expires), ExpectedRevision: authorizationRevision, Traceparent: r.Traceparent, ActorId: actorID, Reason: reason, OperationId: operationID[:], Action: action, DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY}
	payloadType := "synthetic_noop"
	switch p := r.Payload.(type) {
	case SyntheticEchoPayload:
		payloadType = "synthetic_echo"
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: p.Message}}
	case SyntheticNoopPayload:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	case SessionDisconnectPayload:
		payloadType = "session_disconnect"
		envelope.Payload = &agentv1.CommandEnvelope_SessionDisconnect{SessionDisconnect: &agentv1.SessionDisconnect{SessionId: p.SessionID, BootId: p.BootID}}
	case SessionTerminatePayload:
		payloadType = "session_terminate"
		envelope.Payload = &agentv1.CommandEnvelope_SessionTerminate{SessionTerminate: &agentv1.SessionTerminate{SessionId: p.SessionID, BootId: p.BootID}}
	case IPBanRemovePayload:
		payloadType = "ip_ban_remove"
		envelope.Payload = &agentv1.CommandEnvelope_IpBanRemove{IpBanRemove: &agentv1.IpBanRemove{Ip: p.IP}}
	case ServiceReloadPayload:
		payloadType = "service_reload"
		envelope.Payload = &agentv1.CommandEnvelope_ServiceReload{ServiceReload: &agentv1.ServiceReload{}}
	case ConfigPlanPayload:
		payloadType = "config_plan"
		envelope.Payload = &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: p.Candidate, CandidateHash: p.CandidateHash, ExpectedRevision: p.PlanRevision}}
		if p.CompleteCandidate != nil {
			envelope.Payload = &agentv1.CommandEnvelope_CompleteConfigPlan{CompleteConfigPlan: &agentv1.CompleteConfigPlan{Candidate: p.CompleteCandidate, CandidateHash: p.CandidateHash}}
		}
	case ConfigApplyPayload:
		payloadType = "config_apply"
		envelope.Payload = &agentv1.CommandEnvelope_ConfigApply{ConfigApply: &agentv1.ConfigApply{Candidate: p.Candidate, CandidateHash: p.CandidateHash, ExpectedCurrentHash: p.ExpectedCurrentHash, DesiredRevision: p.DesiredRevision}}
		if p.CompleteCandidate != nil {
			envelope.Payload = &agentv1.CommandEnvelope_CompleteConfigApply{CompleteConfigApply: &agentv1.CompleteConfigApply{Candidate: p.CompleteCandidate, CandidateHash: p.CandidateHash, ExpectedCurrentHash: p.ExpectedCurrentHash, MaterializedHash: p.MaterializedHash, DesiredRevision: p.DesiredRevision, PlanId: p.ApplyMetadata.PlanID[:], PlanExpiresAt: p.PlanExpiresAt}}
		}
	case CertificateCSRPayload:
		payloadType = "certificate_csr"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateCsr{CertificateCsr: &agentv1.CertificateCsr{CertificateId: p.CertificateID[:], CommonName: p.CommonName, DnsNames: p.DNSNames, KeyBits: p.KeyBits}}
	case CertificateP12Payload:
		payloadType = "certificate_p12"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: p.CertificateID[:], CertificateChainPem: p.CertificateChain, SealedPasswordV1: p.SealedPassword, ArtifactId: p.ArtifactID[:], CertificateVersion: p.CertificateVersion, ArtifactExpiresAt: timestamppb.New(p.ArtifactMetadata.ExpiresAt)}}
	case CertificateRevokePayload:
		payloadType = "certificate_revoke"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: p.CertificateID[:], Reason: p.RevocationReason, CertificateVersion: p.CertificateVersion}}
	case AgentUpgradePayload:
		payloadType = "agent_upgrade"
		envelope.Payload = &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: &agentv1.AgentUpgrade{TargetVersion: p.TargetVersion, PackageSha256: p.PackageSHA256, Architecture: p.Architecture}}
	default:
		return nil, "", ErrInvalidRequest
	}
	if err := semanticpayload.PopulateV2(envelope); err != nil {
		return nil, "", fmt.Errorf("compute semantic payload hash: %w", err)
	}
	envelope.RequiredCapability = requestCapability(r)
	if envelope.RequiredCapability == "" {
		switch r.Payload.kind() {
		case SyntheticNoop:
			envelope.RequiredCapability = "synthetic.noop"
		case SyntheticEcho:
			envelope.RequiredCapability = "synthetic.echo"
		}
	}
	if err := signer.Authorize(envelope); err != nil {
		return nil, "", fmt.Errorf("authorize typed command: %w", err)
	}
	data, err := proto.Marshal(envelope)
	if err != nil {
		return nil, "", fmt.Errorf("marshal typed command: %w", err)
	}
	return data, payloadType, nil
}

func validDNSName(value string) bool {
	if len(value) < 1 || len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func findIdempotent(ctx context.Context, tx database.Tx, workspaceID uuid.UUID, key string, hash []byte) (Operation, bool, error) {
	store, err := operationstore.FromTransaction(tx)
	if err != nil {
		return Operation{}, false, err
	}
	op, same, err := store.FindIdempotent(ctx, workspaceID, key, hash)
	if err != nil {
		return Operation{}, false, fmt.Errorf("read idempotent operation: %w", err)
	}
	return op, same, nil
}

func newIDs(count int) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, error) {
	ids := make([]uuid.UUID, count)
	for i := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, err
		}
		ids[i] = id
	}
	return ids[0], ids[1], ids[2], ids[3], ids[4], nil
}
func twoIDs() (uuid.UUID, uuid.UUID, error) {
	a, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	b, err := uuid.NewV7()
	return a, b, err
}
func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
