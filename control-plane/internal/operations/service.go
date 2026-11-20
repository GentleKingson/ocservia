package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	tracecontext "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
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
	notify                    chan struct{}
}

// NewBackend supplies the common operation and background Worker lifecycle.
func NewBackend(backend database.Backend, commandLimit int, signer *commandauth.Signer) *Service {
	return &Service{
		backend:                   backend,
		now:                       func() time.Time { return time.Now().UTC() },
		commandLimit:              commandLimit,
		signer:                    signer,
		agentUpgradeReconcileTime: defaultAgentUpgradeReconcileTimeout,
		notify:                    make(chan struct{}, 1),
	}
}

// Notify wakes an attached outbox worker without blocking the caller so newly
// committed commands dispatch immediately instead of waiting for the next poll
// tick.
func (s *Service) Notify() {
	if s == nil || s.notify == nil {
		return
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Wakeup returns the coalesced notification channel for an outbox worker.
func (s *Service) Wakeup() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.notify
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
	intentStore, err := operationstore.FromTransaction(tx)
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
		rolloutObservation, err = s.lockRolloutObservation(ctx, intentStore, request, upgrade, workspaceID, nodeStatus, now, created)
		if err != nil {
			return Operation{}, false, err
		}
	}
	if upgrade.RolloutID == uuid.Nil {
		switch request.Payload.(type) {
		case ConfigApplyPayload, CertificateP12Payload, CertificateRevokePayload, ServiceReloadPayload, AgentUpgradePayload:
			if err := auth.AuthorizeMutationTx(ctx, tx, request.ActorIdentityID, request.ActorSessionID, workspaceID); err != nil {
				return Operation{}, false, err
			}
		}
	}
	if existing, same, err := findIdempotent(ctx, tx, workspaceID, request.IdempotencyKey, hash[:]); err != nil {
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
	if err := validateConfigurationIntent(ctx, tx, intentStore, request, workspaceID, created); err != nil {
		return Operation{}, false, err
	}
	if err := bindCommandCapability(ctx, intentStore, &request); err != nil {
		return Operation{}, false, err
	}
	if err := validateObservedTarget(ctx, intentStore, request); err != nil {
		return Operation{}, false, err
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
	if err := commandlimit.ReserveBacklog(ctx, tx, workspaceID, request.NodeID); err != nil {
		return Operation{}, false, err
	}
	if err := intentStore.InsertIntent(ctx, operationstore.QueuedIntent{
		ID: operationID, WorkspaceID: workspaceID, NodeID: request.NodeID, CommandID: commandID,
		RequestID: request.RequestID, TraceID: tracecontext.TraceID(request.Traceparent), IdempotencyKey: request.IdempotencyKey,
		RequestHash: hash[:], ExpiresAt: expiry, CreatedAt: created,
	}); err != nil {
		return Operation{}, false, fmt.Errorf("insert operation intent: %w", err)
	}
	if err := insertDomainIntent(ctx, tx, intentStore, request, workspaceID, operationID, created, expiry, fromVersion); err != nil {
		return Operation{}, false, err
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
	if err := appendIntentAudit(ctx, tx, request, workspaceID, operationID, commandID, auditID, fromVersion, now); err != nil {
		return Operation{}, false, err
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
			if availableAt == created {
				s.Notify()
			}
			return existing, false, nil
		}
		return Operation{}, false, fmt.Errorf("commit operation transaction: %w", err)
	}
	if availableAt == created {
		s.Notify()
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

// ListEventsWithState returns operation events after cursor and, when no new
// events are present, reads the operation's current state within the same
// transaction so SSE watchers detect already-terminal operations without a
// second transaction or join.
func (s *Service) ListEventsWithState(ctx context.Context, operationID, after uuid.UUID, limit int) ([]Event, string, error) {
	if limit < 1 || limit > 200 {
		return nil, "", ErrInvalidRequest
	}
	var events []Event
	var state string
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		events, err = store.Events(ctx, operationID, after, limit)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			state, err = store.OperationState(ctx, operationID)
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil, "", database.ErrNotFound
		}
		return nil, "", fmt.Errorf("list operation events: %w", err)
	}
	return events, state, nil
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
		if p.PlanMetadata != nil {
			intent.PlanTemplate = p.PlanMetadata.TemplateName
		}
		intent.OcservVersion, intent.PlanCapabilities = p.OcservVersion, p.PlanCapabilities
	case ConfigApplyPayload:
		intent.CompleteProfile, intent.MaterializedHash, intent.PlanExpiresAt = p.CompleteCandidate != nil, hex.EncodeToString(p.MaterializedHash), p.PlanExpiresAt
		intent.CandidateHash, intent.ExpectedCurrentHash = fmt.Sprintf("%x", p.CandidateHash), fmt.Sprintf("%x", p.ExpectedCurrentHash)
		intent.DesiredRevision, intent.PlanRevision = p.DesiredRevision, p.PlanRevision
		if p.ApplyMetadata != nil {
			intent.PlanID = p.ApplyMetadata.PlanID
		}
	case CertificateCSRPayload:
		intent.CommonName, intent.DNSNames, intent.KeyBits = p.CommonName, p.DNSNames, p.KeyBits
	case CertificateP12Payload:
		intent.CertificateID, intent.CertificateVersion, intent.ArtifactID = p.CertificateID, p.CertificateVersion, p.ArtifactID
		intent.CertificateChainHash = hashBytes(p.CertificateChain)
		if p.SealedPassword != nil {
			intent.SealedPasswordHash = hashBytes(p.SealedPassword.GetCiphertext())
			intent.SecretKeyID = p.SealedPassword.GetKeyId()
			intent.SecretVersion = int32(p.SealedPassword.GetVersion())
			intent.SecretPurpose = int32(p.SealedPassword.GetPurpose())
		}
		if p.ArtifactMetadata != nil {
			intent.ArtifactTokenHash = hex.EncodeToString(p.ArtifactMetadata.TokenSHA256)
			intent.ArtifactRequestHash = hex.EncodeToString(p.ArtifactMetadata.RequestHash)
			intent.ArtifactExpiresAt = p.ArtifactMetadata.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
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
		if p.CompleteCandidate == nil {
			return nil, "", ErrInvalidRequest
		}
		payloadType = "config_plan"
		envelope.Payload = &agentv1.CommandEnvelope_CompleteConfigPlan{CompleteConfigPlan: &agentv1.CompleteConfigPlan{Candidate: p.CompleteCandidate, CandidateHash: p.CandidateHash}}
	case ConfigApplyPayload:
		if p.CompleteCandidate == nil {
			return nil, "", ErrInvalidRequest
		}
		payloadType = "config_apply"
		envelope.Payload = &agentv1.CommandEnvelope_CompleteConfigApply{CompleteConfigApply: &agentv1.CompleteConfigApply{Candidate: p.CompleteCandidate, CandidateHash: p.CandidateHash, ExpectedCurrentHash: p.ExpectedCurrentHash, MaterializedHash: p.MaterializedHash, DesiredRevision: p.DesiredRevision, PlanId: p.ApplyMetadata.PlanID[:], PlanExpiresAt: p.PlanExpiresAt}}
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
