package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	configurationstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
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

type SyntheticKind string

const (
	SyntheticNoop     SyntheticKind = "noop"
	SyntheticEcho     SyntheticKind = "echo"
	SessionDisconnect SyntheticKind = "session_disconnect"
	SessionTerminate  SyntheticKind = "session_terminate"
	IPBanRemove       SyntheticKind = "ip_ban_remove"
	ServiceReload     SyntheticKind = "service_reload"
	ConfigPlan        SyntheticKind = "config_plan"
	ConfigApply       SyntheticKind = "config_apply"
	CertificateCSR    SyntheticKind = "certificate_csr"
	CertificateP12    SyntheticKind = "certificate_p12"
	CertificateRevoke SyntheticKind = "certificate_revoke"
	AgentUpgrade      SyntheticKind = "agent_upgrade"
)

type CreateRequest struct {
	NodeID              uuid.UUID
	IdempotencyKey      string
	ExpectedVersion     int64
	Kind                SyntheticKind
	Message             string
	Candidate           []byte
	CandidateHash       []byte
	ExpectedCurrentHash []byte
	DesiredRevision     uint64
	PlanRevision        uint64
	PlanMetadata        *ConfigPlanMetadata
	ApplyMetadata       *ConfigApplyMetadata
	ArtifactMetadata    *ArtifactMetadata
	OcservVersion       string
	PlanCapabilities    []string
	CertificateID       uuid.UUID
	CommonName          string
	DNSNames            []string
	KeyBits             uint32
	CertificateChain    []byte
	SealedPassword      *agentv1.SealedSecretV1
	CertificateVersion  uint64
	ArtifactID          uuid.UUID
	RevocationReason    string
	TargetVersion       string
	PackageSHA256       []byte
	Architecture        string
	FromVersion         string
	// RolloutID binds a generated node upgrade to its durable fleet rollout.
	// The rollout approval was consumed once at rollout creation; dispatch
	// validates against the consumed binding instead of consuming again.
	RolloutID           uuid.UUID
	SessionID           string
	BootID              string
	IP                  string
	ActorID             string
	ActorIdentityID     uuid.UUID
	ActorSessionID      uuid.UUID
	ApprovalID          uuid.UUID
	ApprovalRequestHash []byte
	Action              string
	Reason              string
	SupersedePending    bool
	HoldDispatch        bool
	TTL                 time.Duration
	RequestID           string
	Traceparent         string
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
// catalog used to resolve package digests for rollout-dispatched node
// upgrades. Without it, rollout creation and advancement fail closed.
func (s *Service) EnableReleaseCatalog(catalog *releasecatalog.Catalog) {
	s.releaseCatalog = catalog
}

func (s *Service) CreateSynthetic(ctx context.Context, request CreateRequest) (Operation, bool, error) {
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
	if request.RolloutID != uuid.Nil {
		// Serialize generated operation intent, including idempotent replay,
		// with rollout pause/resume. A cleared claim or non-running rollout
		// must not create or recover an operation from an older memory claim.
		claim, err := intentStore.LockRolloutClaim(ctx, request.RolloutID, workspaceID, request.NodeID)
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
		reason, digest, eligible := s.rolloutNodeEligibility(now, observation, request.TargetVersion)
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
		if observation.Architecture != request.Architecture || !bytes.Equal(digest[:], request.PackageSHA256) {
			return Operation{}, false, ErrStaleRevision
		}
		rolloutObservation = &observation
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
	if request.Kind != SyntheticNoop && request.Kind != SyntheticEcho {
		attestationReady, err := intentStore.AttestationReady(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("recheck privd result attestation: %w", err)
		}
		if !attestationReady {
			return Operation{}, false, ErrCapabilityMissing
		}
	}
	if request.Kind == ConfigPlan {
		configStore, err := configurationstore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		state, err := configStore.State(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("read configuration revision: %w", err)
		}
		if state.Revision < 0 || uint64(state.Revision) != request.PlanRevision {
			return Operation{}, false, ErrStaleRevision
		}
		if state.OcservVersion != request.OcservVersion {
			return Operation{}, false, ErrStaleRevision
		}
		for _, required := range request.PlanCapabilities {
			approved, err := intentStore.HasCapability(ctx, request.NodeID, required)
			if err != nil {
				return Operation{}, false, fmt.Errorf("recheck configuration capability: %w", err)
			}
			if !approved {
				return Operation{}, false, ErrCapabilityMissing
			}
		}
	}
	if request.Kind == ConfigApply {
		configStore, err := configurationstore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		plan, err := configStore.Proof(ctx, request.ApplyMetadata.PlanID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("lock configuration plan: %w", err)
		}
		var validation agentv1.ConfigPlanResult
		if plan.WorkspaceID != workspaceID || plan.NodeID != request.NodeID || plan.ExpectedRevision < 0 || uint64(plan.ExpectedRevision) != request.PlanRevision || !bytes.Equal(plan.CandidateHash, request.CandidateHash) || !plan.ExpiresAt.Valid || plan.ExpiresAt.Micros <= created.Micros || plan.State != "succeeded" || proto.Unmarshal(plan.Result, &validation) != nil || !validation.GetCurrentUnchanged() || !validation.GetStagingCleaned() || !bytes.Equal(validation.GetCandidateHash(), plan.CandidateHash) || !bytes.Equal(validation.GetCurrentHash(), request.ExpectedCurrentHash) {
			return Operation{}, false, ErrStaleRevision
		}
		state, err := configStore.State(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("read configuration apply fence: %w", err)
		}
		if state.Locked || state.Revision != plan.ExpectedRevision || state.DesiredRevision < state.Revision || request.DesiredRevision != uint64(state.DesiredRevision)+1 {
			return Operation{}, false, ErrStaleRevision
		}
		applyActive, err := configStore.HasActiveApply(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check active configuration apply: %w", err)
		}
		if applyActive {
			return Operation{}, false, ErrConfigApplyActive
		}
		if err := approvals.ConsumeBoundTx(ctx, commonTx, request.ApprovalID, workspaceID, request.ActorIdentityID, "config.apply", "config_plan", request.ApplyMetadata.PlanID, plan.CandidateHash); err != nil {
			return Operation{}, false, err
		}
		request.ApprovalRequestHash = append([]byte(nil), plan.CandidateHash...)
	}
	if request.Kind == CertificateP12 || request.Kind == CertificateRevoke {
		if err := approvals.ConsumeBoundTx(ctx, commonTx, request.ApprovalID, workspaceID, request.ActorIdentityID, request.Action, "certificate", request.CertificateID, request.ApprovalRequestHash); err != nil {
			return Operation{}, false, err
		}
	}
	if capability := capabilityFor(request.Kind); capability != "" {
		approved, err := intentStore.HasCapability(ctx, request.NodeID, capability)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check operation capability: %w", err)
		}
		if !approved {
			return Operation{}, false, ErrCapabilityMissing
		}
	}
	if request.Kind == SessionDisconnect || request.Kind == SessionTerminate {
		present, err := intentStore.HasSession(ctx, request.NodeID, request.SessionID, request.BootID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check observed session: %w", err)
		}
		if !present {
			return Operation{}, false, ErrTargetNotObserved
		}
	}
	if request.Kind == IPBanRemove {
		present, err := intentStore.HasIPBan(ctx, request.NodeID, request.IP)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check observed IP ban: %w", err)
		}
		if !present {
			return Operation{}, false, ErrTargetNotObserved
		}
	}
	if request.Kind == ServiceReload {
		approvalHash, _ := approvals.GenericBinding(request.Action, "node", request.NodeID)
		if err := approvals.ConsumeBoundTx(ctx, commonTx, request.ApprovalID, workspaceID, request.ActorIdentityID, request.Action, "node", request.NodeID, approvalHash); err != nil {
			return Operation{}, false, err
		}
		request.ApprovalRequestHash = approvalHash
	}
	if request.Kind == AgentUpgrade {
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
		if telemetry.ClassifyAgentVersion(observedVersion, request.TargetVersion) != telemetry.AgentVersionStateUpgradeAvailable {
			return Operation{}, false, ErrStaleRevision
		}
		request.FromVersion = observedVersion
		if request.RolloutID != uuid.Nil {
			// The rollout approval bound the immutable rollout request and was
			// consumed when the rollout was created; every generated node
			// upgrade validates against that consumed binding.
			if err := approvals.ValidateConsumedBoundTx(ctx, commonTx, request.ApprovalID, workspaceID, request.ActorIdentityID, "agent.rollout", "batch_operation", request.RolloutID, request.ApprovalRequestHash); err != nil {
				return Operation{}, false, err
			}
		} else {
			approvalHash, _ := approvals.AgentUpgradeBinding(request.NodeID, request.TargetVersion, request.PackageSHA256, request.Architecture)
			if err := approvals.ConsumeBoundTx(ctx, commonTx, request.ApprovalID, workspaceID, request.ActorIdentityID, request.Action, "node", request.NodeID, approvalHash); err != nil {
				return Operation{}, false, err
			}
			request.ApprovalRequestHash = approvalHash
		}
		// The release identity is approved; only one scheduled upgrade may
		// advance per node, so a second concurrent attempt fails closed
		// before any durable intent is written.
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
		RequestID: request.RequestID, TraceID: traceID(request.Traceparent), IdempotencyKey: request.IdempotencyKey,
		RequestHash: hash[:], ExpiresAt: expiry, CreatedAt: created,
	}); err != nil {
		return Operation{}, false, fmt.Errorf("insert operation intent: %w", err)
	}
	if request.Kind == ConfigPlan {
		warnings, err := json.Marshal(request.PlanMetadata.Warnings)
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
		if err := configStore.InsertPlan(ctx, configurationstore.PendingPlan{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, TemplateName: request.PlanMetadata.TemplateName, ExpectedRevision: request.PlanRevision, CandidateHash: request.CandidateHash, CandidateRedacted: request.PlanMetadata.CandidateRedacted, Warnings: logicalWarnings, ExpiresAt: expiry, CreatedBy: optionalUUID(request.PlanMetadata.CreatedBy), CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert configuration plan: %w", err)
		}
	}
	if request.Kind == ConfigApply {
		configStore, err := configurationstore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		if err := configStore.InsertApply(ctx, configurationstore.PendingApply{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, PlanID: request.ApplyMetadata.PlanID, ApprovalID: request.ApprovalID, ExpectedRevision: request.PlanRevision, DesiredRevision: request.DesiredRevision, CandidateHash: request.CandidateHash, PreviousHash: request.ExpectedCurrentHash, CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert configuration apply: %w", err)
		}
		advanced, err := configStore.AdvanceDesiredRevision(ctx, request.NodeID, request.DesiredRevision, created)
		if err != nil {
			return Operation{}, false, fmt.Errorf("advance configuration desired revision: %w", err)
		}
		if !advanced {
			return Operation{}, false, ErrStaleRevision
		}
	}
	if request.Kind == CertificateCSR {
		dnsNames, err := json.Marshal(request.DNSNames)
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
		if err := store.InsertCSR(ctx, certificatestore.PendingCSR{ID: request.CertificateID, WorkspaceID: workspaceID, NodeID: request.NodeID, OperationID: operationID, CommonName: request.CommonName, DNSNames: names, KeyBits: request.KeyBits, CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert certificate request: %w", err)
		}
	}
	if request.Kind == CertificateP12 {
		store, err := certificatestore.FromTransaction(commonTx)
		if err != nil {
			return Operation{}, false, err
		}
		artifactExpiry, err := value.FromTime(request.ArtifactMetadata.ExpiresAt)
		if err != nil {
			return Operation{}, false, err
		}
		if err := store.InsertArtifact(ctx, certificatestore.PendingArtifact{ID: request.ArtifactID, WorkspaceID: workspaceID, NodeID: request.NodeID, CertificateID: request.CertificateID, CertificateVersion: request.CertificateVersion, OperationID: operationID, TokenSHA256: request.ArtifactMetadata.TokenSHA256, RequestHash: request.ArtifactMetadata.RequestHash, ExpiresAt: artifactExpiry, CreatedAt: created, ApprovalID: request.ApprovalID}); err != nil {
			return Operation{}, false, fmt.Errorf("insert certificate artifact operation: %w", err)
		}
	}
	if request.Kind == AgentUpgrade {
		if err := intentStore.InsertUpgrade(ctx, operationstore.PendingUpgrade{ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, TargetVersion: request.TargetVersion, PackageSHA256: request.PackageSHA256, Architecture: request.Architecture, FromVersion: request.FromVersion, ApprovalID: optionalUUID(request.ApprovalID), CreatedAt: created}); err != nil {
			return Operation{}, false, fmt.Errorf("insert agent upgrade operation: %w", err)
		}
	}
	availableAt := created
	if request.HoldDispatch {
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
	if request.Kind == ConfigPlan {
		auditSummary, _ = json.Marshal(map[string]any{"candidate_hash": fmt.Sprintf("%x", request.CandidateHash), "expected_revision": request.PlanRevision})
	}
	if request.Kind == ConfigApply {
		auditSummary, _ = json.Marshal(map[string]any{"plan_id": request.ApplyMetadata.PlanID, "candidate_hash": fmt.Sprintf("%x", request.CandidateHash), "previous_hash": fmt.Sprintf("%x", request.ExpectedCurrentHash), "desired_revision": request.DesiredRevision})
	}
	if request.Kind == AgentUpgrade {
		summary := map[string]any{"from_version": request.FromVersion, "target_version": request.TargetVersion, "architecture": request.Architecture, "package_sha256": fmt.Sprintf("%x", request.PackageSHA256)}
		if request.RolloutID != uuid.Nil {
			summary["rollout_id"] = request.RolloutID
		}
		auditSummary, _ = json.Marshal(summary)
	}
	if err := audit.AppendChainTx(ctx, commonTx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "user", ActorID: actorID, SessionID: optionalUUID(request.ActorSessionID), Action: action, ResourceType: "operation", ResourceID: operationID, NodeID: &request.NodeID, CommandID: &commandID, ApprovalID: optionalUUID(request.ApprovalID), RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: reason, AfterSummary: auditSummary, At: now}); err != nil {
		return Operation{}, false, fmt.Errorf("append operation audit intent: %w", err)
	}
	if err := intentStore.NotifyOutbox(ctx, outboxID); err != nil {
		return Operation{}, false, fmt.Errorf("notify outbox worker: %w", err)
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
	if request.Kind == ConfigApply {
		operation.ConfigApplyState = "queued"
	}
	if request.Kind == AgentUpgrade {
		operation.AgentUpgradeState = "queued"
		operation.AgentUpgradeTarget = request.TargetVersion
	}
	return operation, false, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Operation, error) {
	var result Operation
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		result, err = store.Get(ctx, id)
		return err
	})
	return result, err
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

func validateCreate(r CreateRequest) error {
	if r.NodeID == uuid.Nil || r.NodeID.Version() != 7 || r.ExpectedVersion < 1 || r.RequestID == "" || !validTraceparent(r.Traceparent) || r.TTL < time.Second || r.TTL > 24*time.Hour {
		return ErrInvalidRequest
	}
	if len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 || strings.TrimSpace(r.IdempotencyKey) != r.IdempotencyKey {
		return ErrInvalidRequest
	}
	if len(r.ApprovalRequestHash) != 0 && r.Kind != CertificateP12 && r.Kind != CertificateRevoke && !(r.Kind == AgentUpgrade && r.RolloutID != uuid.Nil) {
		return ErrInvalidRequest
	}
	if r.Kind == AgentUpgrade && r.RolloutID == uuid.Nil && len(r.ApprovalRequestHash) != 0 {
		return ErrInvalidRequest
	}
	if r.RolloutID != uuid.Nil && (r.RolloutID.Version() != 7 || r.Kind != AgentUpgrade || len(r.ApprovalRequestHash) != sha256.Size) {
		return ErrInvalidRequest
	}
	if r.Kind != SyntheticNoop && r.Kind != SyntheticEcho && r.Kind != SessionDisconnect && r.Kind != SessionTerminate && r.Kind != IPBanRemove && r.Kind != ServiceReload && r.Kind != ConfigPlan && r.Kind != ConfigApply && r.Kind != CertificateCSR && r.Kind != CertificateP12 && r.Kind != CertificateRevoke && r.Kind != AgentUpgrade {
		return ErrInvalidRequest
	}
	if r.Kind == SyntheticNoop && r.Message != "" || len(r.Message) > 4096 {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigPlan && (len(r.Candidate) == 0 || len(r.Candidate) > 256*1024 || len(r.CandidateHash) != sha256.Size || len(r.ExpectedCurrentHash) != 0 || r.DesiredRevision != 0 || r.ApplyMetadata != nil || r.PlanRevision > uint64(^uint64(0)>>1) || r.PlanMetadata == nil || strings.TrimSpace(r.PlanMetadata.TemplateName) == "" || len(r.PlanMetadata.TemplateName) > 128 || len(r.PlanMetadata.CandidateRedacted) == 0 || len(r.PlanMetadata.CandidateRedacted) > 256*1024 || len(r.PlanCapabilities) == 0) {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigPlan {
		digest := sha256.Sum256(r.Candidate)
		if !bytes.Equal(digest[:], r.CandidateHash) {
			return ErrInvalidRequest
		}
	}
	if r.Kind == ConfigApply && (len(r.Candidate) == 0 || len(r.Candidate) > 256*1024 || len(r.CandidateHash) != sha256.Size || len(r.ExpectedCurrentHash) != sha256.Size || r.PlanRevision > uint64(^uint64(0)>>1) || r.DesiredRevision <= r.PlanRevision || r.ApplyMetadata == nil || r.ApplyMetadata.PlanID == uuid.Nil) {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigApply {
		digest := sha256.Sum256(r.Candidate)
		if !bytes.Equal(digest[:], r.CandidateHash) || r.ActorID == "" || r.ActorIdentityID == uuid.Nil || r.ActorSessionID == uuid.Nil || r.ApprovalID == uuid.Nil {
			return ErrInvalidRequest
		}
	}
	if r.Kind == CertificateCSR {
		if r.CertificateID == uuid.Nil || r.CertificateID.Version() != 7 || !validDNSName(r.CommonName) || len(r.DNSNames) > 32 || (r.KeyBits != 2048 && r.KeyBits != 3072 && r.KeyBits != 4096) {
			return ErrInvalidRequest
		}
		for _, name := range r.DNSNames {
			if !validDNSName(name) {
				return ErrInvalidRequest
			}
		}
	}
	if r.Kind == CertificateP12 && (r.CertificateID == uuid.Nil || r.CertificateID.Version() != 7 || r.CertificateVersion == 0 || r.ArtifactID == uuid.Nil || r.ArtifactID.Version() != 7 || len(r.CertificateChain) < 64 || len(r.CertificateChain) > 256*1024 || !validSealedP12(r.SealedPassword) || r.ArtifactMetadata == nil || len(r.ArtifactMetadata.TokenSHA256) != sha256.Size || len(r.ArtifactMetadata.RequestHash) != sha256.Size || !r.ArtifactMetadata.ExpiresAt.After(time.Now().UTC()) || r.ArtifactMetadata.ExpiresAt.After(time.Now().UTC().Add(30*time.Minute))) {
		return ErrInvalidRequest
	}
	if (r.Kind == CertificateP12 || r.Kind == CertificateRevoke) && (r.ApprovalID == uuid.Nil || r.ActorIdentityID == uuid.Nil || len(r.ApprovalRequestHash) != sha256.Size) {
		return ErrInvalidRequest
	}
	if r.Kind == CertificateRevoke && (r.CertificateID == uuid.Nil || r.CertificateID.Version() != 7 || r.CertificateVersion == 0 || strings.TrimSpace(r.RevocationReason) == "" || len(r.RevocationReason) > 128) {
		return ErrInvalidRequest
	}
	if r.HoldDispatch && r.Kind != CertificateRevoke {
		return ErrInvalidRequest
	}
	if r.Kind != ConfigPlan && r.Kind != ConfigApply && (len(r.Candidate) != 0 || len(r.CandidateHash) != 0 || len(r.ExpectedCurrentHash) != 0 || r.DesiredRevision != 0 || r.PlanRevision != 0 || r.PlanMetadata != nil || r.ApplyMetadata != nil || r.OcservVersion != "" || len(r.PlanCapabilities) != 0) {
		return ErrInvalidRequest
	}
	if r.Kind == AgentUpgrade && (!semanticpayload.ValidAgentUpgradeTargetVersion(r.TargetVersion) || len(r.PackageSHA256) != sha256.Size || !semanticpayload.ValidAgentUpgradeArchitecture(r.Architecture)) {
		return ErrInvalidRequest
	}
	if r.Kind != AgentUpgrade && (r.TargetVersion != "" || len(r.PackageSHA256) != 0 || r.Architecture != "") {
		return ErrInvalidRequest
	}
	if r.Kind == SessionDisconnect || r.Kind == SessionTerminate {
		sessionID, err := strconv.ParseUint(r.SessionID, 10, 64)
		if err != nil || sessionID == 0 || strconv.FormatUint(sessionID, 10) != r.SessionID {
			return ErrInvalidRequest
		}
		bootID, err := uuid.Parse(r.BootID)
		if err != nil || bootID == uuid.Nil {
			return ErrInvalidRequest
		}
	}
	if r.Kind == IPBanRemove {
		parsed := net.ParseIP(r.IP)
		if parsed == nil || parsed.String() != r.IP {
			return ErrInvalidRequest
		}
	}
	if (r.Kind == SessionDisconnect || r.Kind == SessionTerminate || r.Kind == IPBanRemove || r.Kind == ServiceReload || r.Kind == ConfigPlan || r.Kind == ConfigApply || r.Kind == CertificateCSR || r.Kind == CertificateP12 || r.Kind == CertificateRevoke || r.Kind == AgentUpgrade) && (r.Action == "" || r.Reason == "" || len(r.Reason) > 512) {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigPlan && r.ActorID == "" {
		return ErrInvalidRequest
	}
	if r.Kind == ServiceReload && (r.ActorID == "" || r.ActorIdentityID == uuid.Nil || r.ActorSessionID == uuid.Nil || r.ApprovalID == uuid.Nil) {
		return ErrInvalidRequest
	}
	// Upgrades replace root-owned agent binaries, so they demand the same
	// independent approval boundary as other high-impact node operations.
	if r.Kind == AgentUpgrade && (r.ActorID == "" || r.ActorIdentityID == uuid.Nil || r.ActorSessionID == uuid.Nil || r.ApprovalID == uuid.Nil) {
		return ErrInvalidRequest
	}
	return nil
}

func requestHash(r CreateRequest) [32]byte {
	actorID, action, reason := normalizedAuditIntent(r)
	// Request and trace IDs identify an attempt, not the mutation intent. Every
	// field that selects the target, effect, authorization action, actor, audit
	// reason, revision, or delivery behavior is deliberately bound here.
	intent := struct {
		NodeID               uuid.UUID     `json:"node_id"`
		Kind                 SyntheticKind `json:"kind"`
		Message              string        `json:"message"`
		SessionID            string        `json:"session_id"`
		BootID               string        `json:"boot_id"`
		IP                   string        `json:"ip"`
		ExpectedVersion      int64         `json:"expected_version"`
		SupersedePending     bool          `json:"supersede_pending"`
		HoldDispatch         bool          `json:"hold_dispatch,omitempty"`
		TTLSeconds           int64         `json:"ttl_seconds"`
		ActorID              string        `json:"actor_id"`
		Action               string        `json:"action"`
		Reason               string        `json:"reason"`
		ActorSessionID       uuid.UUID     `json:"actor_session_id"`
		ActorIdentityID      uuid.UUID     `json:"actor_identity_id"`
		ApprovalID           uuid.UUID     `json:"approval_id"`
		CandidateHash        string        `json:"candidate_hash"`
		ExpectedCurrentHash  string        `json:"expected_current_hash"`
		DesiredRevision      uint64        `json:"desired_revision"`
		PlanID               uuid.UUID     `json:"plan_id"`
		PlanRevision         uint64        `json:"plan_revision"`
		PlanTemplate         string        `json:"plan_template"`
		OcservVersion        string        `json:"ocserv_version"`
		PlanCapabilities     []string      `json:"plan_capabilities"`
		CertificateID        uuid.UUID     `json:"certificate_id"`
		CommonName           string        `json:"common_name"`
		DNSNames             []string      `json:"dns_names"`
		KeyBits              uint32        `json:"key_bits"`
		ArtifactID           uuid.UUID     `json:"artifact_id"`
		CertificateChainHash string        `json:"certificate_chain_hash"`
		SealedPasswordHash   string        `json:"sealed_password_hash"`
		SecretKeyID          string        `json:"secret_key_id"`
		SecretVersion        int32         `json:"secret_version"`
		SecretPurpose        int32         `json:"secret_purpose"`
		CertificateVersion   uint64        `json:"certificate_version"`
		ArtifactTokenHash    string        `json:"artifact_token_hash"`
		ArtifactRequestHash  string        `json:"artifact_request_hash"`
		ArtifactExpiresAt    string        `json:"artifact_expires_at"`
		RevocationReason     string        `json:"revocation_reason"`
		TargetVersion        string        `json:"target_version"`
		PackageSHA256        string        `json:"package_sha256"`
		Architecture         string        `json:"architecture"`
	}{NodeID: r.NodeID, Kind: r.Kind, Message: r.Message, SessionID: r.SessionID, BootID: r.BootID, IP: r.IP,
		ExpectedVersion: r.ExpectedVersion, SupersedePending: r.SupersedePending, HoldDispatch: r.HoldDispatch, TTLSeconds: int64(r.TTL / time.Second),
		ActorID: actorID, Action: action, Reason: reason, ActorSessionID: r.ActorSessionID, ActorIdentityID: r.ActorIdentityID,
		ApprovalID: r.ApprovalID, CandidateHash: fmt.Sprintf("%x", r.CandidateHash), ExpectedCurrentHash: fmt.Sprintf("%x", r.ExpectedCurrentHash),
		DesiredRevision: r.DesiredRevision, PlanID: applyPlanID(r.ApplyMetadata), PlanRevision: r.PlanRevision,
		PlanTemplate: planTemplate(r.PlanMetadata), OcservVersion: r.OcservVersion, PlanCapabilities: r.PlanCapabilities,
		CertificateID: idempotencyCertificateID(r), CommonName: r.CommonName, DNSNames: r.DNSNames, KeyBits: r.KeyBits, ArtifactID: r.ArtifactID,
		CertificateChainHash: hashBytes(r.CertificateChain), SealedPasswordHash: sealedPasswordHash(r.SealedPassword), SecretKeyID: sealedPasswordKeyID(r.SealedPassword), SecretVersion: sealedPasswordVersion(r.SealedPassword), SecretPurpose: sealedPasswordPurpose(r.SealedPassword), CertificateVersion: r.CertificateVersion,
		ArtifactTokenHash: artifactTokenHash(r.ArtifactMetadata), ArtifactRequestHash: artifactRequestHash(r.ArtifactMetadata), ArtifactExpiresAt: artifactExpiry(r.ArtifactMetadata),
		RevocationReason: r.RevocationReason, TargetVersion: r.TargetVersion, PackageSHA256: fmt.Sprintf("%x", r.PackageSHA256), Architecture: r.Architecture}
	encoded, err := json.Marshal(intent)
	if err != nil {
		panic("marshal fixed idempotency intent: " + err.Error())
	}
	return sha256.Sum256(encoded)
}

func idempotencyCertificateID(request CreateRequest) uuid.UUID {
	if request.Kind == CertificateCSR {
		return uuid.Nil
	}
	return request.CertificateID
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
	if r.ApprovalID != uuid.Nil {
		envelope.ApprovalId = r.ApprovalID[:]
	}
	if len(r.ApprovalRequestHash) != 0 {
		envelope.ApprovalRequestSha256 = append([]byte(nil), r.ApprovalRequestHash...)
	}
	payloadType := "synthetic_noop"
	switch r.Kind {
	case SyntheticEcho:
		payloadType = "synthetic_echo"
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: r.Message}}
	case SyntheticNoop:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	case SessionDisconnect:
		payloadType = "session_disconnect"
		envelope.Payload = &agentv1.CommandEnvelope_SessionDisconnect{SessionDisconnect: &agentv1.SessionDisconnect{SessionId: r.SessionID, BootId: r.BootID}}
	case SessionTerminate:
		payloadType = "session_terminate"
		envelope.Payload = &agentv1.CommandEnvelope_SessionTerminate{SessionTerminate: &agentv1.SessionTerminate{SessionId: r.SessionID, BootId: r.BootID}}
	case IPBanRemove:
		payloadType = "ip_ban_remove"
		envelope.Payload = &agentv1.CommandEnvelope_IpBanRemove{IpBanRemove: &agentv1.IpBanRemove{Ip: r.IP}}
	case ServiceReload:
		payloadType = "service_reload"
		envelope.Payload = &agentv1.CommandEnvelope_ServiceReload{ServiceReload: &agentv1.ServiceReload{}}
	case ConfigPlan:
		payloadType = "config_plan"
		envelope.Payload = &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: r.Candidate, CandidateHash: r.CandidateHash, ExpectedRevision: r.PlanRevision}}
	case ConfigApply:
		payloadType = "config_apply"
		envelope.Payload = &agentv1.CommandEnvelope_ConfigApply{ConfigApply: &agentv1.ConfigApply{Candidate: r.Candidate, CandidateHash: r.CandidateHash, ExpectedCurrentHash: r.ExpectedCurrentHash, DesiredRevision: r.DesiredRevision}}
	case CertificateCSR:
		payloadType = "certificate_csr"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateCsr{CertificateCsr: &agentv1.CertificateCsr{CertificateId: r.CertificateID[:], CommonName: r.CommonName, DnsNames: r.DNSNames, KeyBits: r.KeyBits}}
	case CertificateP12:
		payloadType = "certificate_p12"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: r.CertificateID[:], CertificateChainPem: r.CertificateChain, SealedPasswordV1: r.SealedPassword, ArtifactId: r.ArtifactID[:], CertificateVersion: r.CertificateVersion, ArtifactExpiresAt: timestamppb.New(r.ArtifactMetadata.ExpiresAt)}}
	case CertificateRevoke:
		payloadType = "certificate_revoke"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: r.CertificateID[:], Reason: r.RevocationReason, CertificateVersion: r.CertificateVersion}}
	case AgentUpgrade:
		payloadType = "agent_upgrade"
		envelope.Payload = &agentv1.CommandEnvelope_AgentUpgrade{AgentUpgrade: &agentv1.AgentUpgrade{TargetVersion: r.TargetVersion, PackageSha256: r.PackageSHA256, Architecture: r.Architecture}}
	}
	if err := semanticpayload.PopulateV2(envelope); err != nil {
		return nil, "", fmt.Errorf("compute semantic payload hash: %w", err)
	}
	envelope.RequiredCapability = capabilityFor(r.Kind)
	if envelope.RequiredCapability == "" {
		switch r.Kind {
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

func capabilityFor(kind SyntheticKind) string {
	switch kind {
	case SessionDisconnect:
		return "ocserv.session.disconnect"
	case SessionTerminate:
		return "ocserv.session.terminate"
	case IPBanRemove:
		return "ocserv.ip_ban.remove"
	case ServiceReload:
		return "ocserv.service.reload"
	case ConfigPlan:
		return "ocserv.config.plan"
	case ConfigApply:
		return "ocserv.config.apply"
	case CertificateCSR, CertificateP12:
		return "ocserv.certificate.issue"
	case CertificateRevoke:
		return "ocserv.certificate.revoke"
	case AgentUpgrade:
		// v2 is fence-capable: the source runner executes the upgrade with
		// the execution-time downgrade fence and installation commit record.
		// Scheduling from a v1-only node would run the first hop unprotected.
		return "ocserv.agent.upgrade.v2"
	default:
		return ""
	}
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
func traceID(value string) string { return value[3:35] }
func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
func validTraceparent(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	for _, p := range parts[1:] {
		for _, c := range p {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return parts[1] != "00000000000000000000000000000000" && parts[2] != "0000000000000000"
}
