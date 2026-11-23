package operations

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/idempotency"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrInvalidRequest      = errors.New("invalid operation request")
	ErrIdempotencyConflict = operationstore.ErrIdempotencyConflict
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
	lastSummary               atomic.Pointer[cachedSummary]
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
	if csr, ok := request.Payload.(CertificateCSRPayload); ok && csr.DNSNames == nil {
		// Use the same array for idempotency and the JSONB certificate record.
		csr.DNSNames = []string{}
		request.Payload = csr
	}
	now := s.now().UTC()
	digest := requestDigest(request)
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
		observation, err := s.lockRolloutObservation(ctx, intentStore, request, upgrade, workspaceID, nodeStatus, now)
		if err != nil {
			return Operation{}, false, err
		}
		fromVersion = observation.AgentVersion
	}
	if upgrade.RolloutID == uuid.Nil {
		if err := auth.AuthorizeMutationTx(ctx, tx, request.ActorIdentityID, request.ActorSessionID, workspaceID); err != nil {
			return Operation{}, false, err
		}
	}
	_, configPlan := request.Payload.(ConfigPlanPayload)
	if configPlan {
		if err := bindCommandCapability(ctx, intentStore, &request); err != nil {
			return Operation{}, false, err
		}
	}
	if existing, err := ReplayTx(ctx, tx, intentStore, workspaceID, request.IdempotencyKey, digest); err != nil || existing.ID != "" {
		return existing, err == nil, err
	}
	if nodeVersion != request.ExpectedVersion {
		return Operation{}, false, ErrStaleRevision
	}
	if request.Payload.kind() != SyntheticNoop && request.Payload.kind() != SyntheticEcho {
		if err := RequireAttestation(ctx, intentStore, request.NodeID); err != nil {
			return Operation{}, false, err
		}
	}
	if err := validateConfigurationIntent(ctx, tx, request, workspaceID, now); err != nil {
		return Operation{}, false, err
	}
	if !configPlan {
		if err := bindCommandCapability(ctx, intentStore, &request); err != nil {
			return Operation{}, false, err
		}
	}
	if err := validateObservedTarget(ctx, intentStore, request); err != nil {
		return Operation{}, false, err
	}
	if _, ok := request.Payload.(AgentUpgradePayload); ok && upgrade.RolloutID == uuid.Nil {
		observed, err := intentStore.LockAgentObservation(ctx, request.NodeID)
		if err != nil && !errors.Is(err, database.ErrNotFound) {
			return Operation{}, false, fmt.Errorf("read observed agent version: %w", err)
		}
		if err == nil {
			fromVersion = observed.AgentVersion
		}
		upgradeActive, err := intentStore.HasActiveUpgrade(ctx, request.NodeID)
		if err != nil {
			return Operation{}, false, fmt.Errorf("check active agent upgrade: %w", err)
		}
		if upgradeActive {
			return Operation{}, false, ErrUpgradeActive
		}
	}

	expiresAt := now.Add(request.TTL)
	availableAt := now
	if payload, ok := request.Payload.(CertificateRevokePayload); ok && payload.HoldDispatch {
		availableAt = expiresAt
	}
	operation, err := s.EnqueueTx(ctx, tx, intentStore, QueueIntent{
		WorkspaceID: workspaceID, NodeID: request.NodeID, Key: request.IdempotencyKey, RequestID: request.RequestID,
		Traceparent: request.Traceparent, Digest: digest, Now: now, Expires: expiresAt, Available: availableAt,
		ExpectedVersion: request.ExpectedVersion,
	}, func(ids QueueIDs) ([]byte, string, error) {
		envelope, payloadType, err := marshalEnvelope(request, ids.Operation, ids.Command, authorizationRevision, now, expiresAt, s.signer)
		if err == nil && request.SupersedePending {
			err = intentStore.SupersedePending(ctx, request.NodeID, payloadType, now)
		}
		return envelope, payloadType, err
	}, func(ids QueueIDs) error {
		return insertDomainIntent(ctx, tx, intentStore, request, workspaceID, ids.Operation, now, expiresAt, fromVersion)
	}, func(ids QueueIDs) error {
		return appendIntentAudit(ctx, tx, request, workspaceID, ids.Operation, ids.Command, ids.Audit, fromVersion, now)
	})
	if err != nil {
		return Operation{}, false, err
	}
	if _, ok := request.Payload.(ConfigApplyPayload); ok && operation.ConfigApplyState == "" {
		operation.ConfigApplyState = "queued"
	}
	if payload, ok := request.Payload.(AgentUpgradePayload); ok && operation.AgentUpgradeState == "" {
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

func requestDigest(r CreateRequest) idempotency.Digest {
	if p, ok := r.Payload.(CertificateP12Payload); ok {
		return P12Intent(r, p)
	}
	actorID, action, reason := normalizedAuditIntent(r)
	// Request and trace IDs identify an attempt, not the mutation intent. Every
	// field that selects the target, effect, authorization action, actor, audit
	// reason, revision, or delivery behavior is deliberately bound here.
	intent := struct {
		CompleteProfile     bool                   `json:"complete_profile,omitempty"`
		MaterializedHash    string                 `json:"materialized_hash,omitempty"`
		PlanExpiresAt       *timestamppb.Timestamp `json:"plan_expires_at,omitempty"`
		NodeID              uuid.UUID              `json:"node_id"`
		Kind                Kind                   `json:"kind"`
		Message             string                 `json:"message"`
		SessionID           string                 `json:"session_id"`
		BootID              string                 `json:"boot_id"`
		IP                  string                 `json:"ip"`
		ExpectedVersion     int64                  `json:"expected_version"`
		SupersedePending    bool                   `json:"supersede_pending"`
		HoldDispatch        bool                   `json:"hold_dispatch,omitempty"`
		TTLSeconds          int64                  `json:"ttl_seconds"`
		ActorID             string                 `json:"actor_id"`
		Action              string                 `json:"action"`
		Reason              string                 `json:"reason"`
		ActorSessionID      uuid.UUID              `json:"actor_session_id"`
		ActorIdentityID     uuid.UUID              `json:"actor_identity_id"`
		CandidateHash       string                 `json:"candidate_hash"`
		ExpectedCurrentHash string                 `json:"expected_current_hash"`
		DesiredRevision     uint64                 `json:"desired_revision"`
		PlanID              uuid.UUID              `json:"plan_id"`
		PlanRevision        uint64                 `json:"plan_revision"`
		PlanTemplate        string                 `json:"plan_template"`
		OcservVersion       string                 `json:"ocserv_version"`
		PlanCapabilities    []string               `json:"plan_capabilities"`
		CertificateID       uuid.UUID              `json:"certificate_id"`
		CommonName          string                 `json:"common_name"`
		DNSNames            []string               `json:"dns_names"`
		KeyBits             uint32                 `json:"key_bits"`
		ArtifactID          uuid.UUID              `json:"artifact_id"`
		CertificateVersion  uint64                 `json:"certificate_version"`
		RevocationReason    string                 `json:"revocation_reason"`
		RolloutID           uuid.UUID              `json:"-" cbor:"rollout_id,omitempty"`
		TargetVersion       string                 `json:"target_version"`
		PackageSHA256       string                 `json:"package_sha256"`
		Architecture        string                 `json:"architecture"`
	}{NodeID: r.NodeID, Kind: r.Payload.kind(), ExpectedVersion: r.ExpectedVersion,
		SupersedePending: r.SupersedePending, TTLSeconds: int64(r.TTL / time.Second),
		ActorID: actorID, Action: action, Reason: reason, ActorSessionID: r.ActorSessionID,
		ActorIdentityID: r.ActorIdentityID}
	// Keep this projection stable for persisted CBOR receipts.
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
		intent.CompleteProfile = true
		intent.CandidateHash, intent.PlanRevision = fmt.Sprintf("%x", p.Candidate.Digest()), p.Candidate.Revision()
		if p.PlanMetadata != nil {
			intent.PlanTemplate = p.PlanMetadata.TemplateName
		}
		intent.OcservVersion, intent.PlanCapabilities = p.OcservVersion, []string{configprofile.PlanCapability}
	case ConfigApplyPayload:
		intent.CompleteProfile, intent.MaterializedHash, intent.PlanExpiresAt = true, hex.EncodeToString(p.MaterializedHash), p.PlanExpiresAt
		intent.CandidateHash, intent.ExpectedCurrentHash = fmt.Sprintf("%x", p.Candidate.Digest()), fmt.Sprintf("%x", p.ExpectedCurrentHash)
		intent.DesiredRevision, intent.PlanRevision = p.DesiredRevision, p.Candidate.Revision()
		if p.ApplyMetadata != nil {
			intent.PlanID = p.ApplyMetadata.PlanID
		}
	case CertificateCSRPayload:
		intent.CommonName, intent.DNSNames, intent.KeyBits = p.CommonName, p.DNSNames, p.KeyBits
	case CertificateRevokePayload:
		intent.CertificateID, intent.CertificateVersion, intent.RevocationReason, intent.HoldDispatch = p.CertificateID, p.CertificateVersion, p.RevocationReason, p.HoldDispatch
	case AgentUpgradePayload:
		intent.TargetVersion, intent.PackageSHA256, intent.Architecture = p.TargetVersion, fmt.Sprintf("%x", p.PackageSHA256), p.Architecture
		intent.RolloutID = p.RolloutID
	}

	return idempotency.New("operation", intent)
}

// P12Intent binds the request to the certificate version and artifact ID.
// The generated credentials, ciphertext, and expiry belong to the first successful enqueue.
func P12Intent(r CreateRequest, p CertificateP12Payload) idempotency.Digest {
	intent := map[string]any{
		"certificate_id": p.CertificateID, "certificate_version": p.CertificateVersion,
		"artifact_request_id": p.ArtifactID, "node_id": r.NodeID, "expected_version": r.ExpectedVersion,
		"actor_identity_id": r.ActorIdentityID, "actor_session_id": r.ActorSessionID, "reason": r.Reason,
	}
	return idempotency.New("certificate-p12", intent)
}

func validSealedP12(secret *agentv1.SealedSecretV1) bool {
	return secret != nil && secret.GetVersion() == 1 && secret.GetPurpose() == agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD && strings.TrimSpace(secret.GetKeyId()) != "" && len(secret.GetKeyId()) <= 128 && len(secret.GetCiphertext()) >= 32 && len(secret.GetCiphertext()) <= 16*1024
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
	actorID, action, reason := normalizedAuditIntent(r)
	envelope, err := NewCommandEnvelope(r.NodeID, operationID, commandID, authorizationRevision, now, expires, r.Traceparent, actorID, action, reason)
	if err != nil {
		return nil, "", err
	}
	payloadType := "synthetic_noop"
	var candidate configprofile.Normalized
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
		candidate = p.Candidate
		payloadType = "config_plan"
		envelope.Payload = &agentv1.CommandEnvelope_CompleteConfigPlan{CompleteConfigPlan: &agentv1.CompleteConfigPlan{}}
	case ConfigApplyPayload:
		candidate = p.Candidate
		payloadType = "config_apply"
		envelope.Payload = &agentv1.CommandEnvelope_CompleteConfigApply{CompleteConfigApply: &agentv1.CompleteConfigApply{ExpectedCurrentHash: p.ExpectedCurrentHash, MaterializedHash: p.MaterializedHash, DesiredRevision: p.DesiredRevision, PlanId: p.ApplyMetadata.PlanID[:], PlanExpiresAt: p.PlanExpiresAt}}
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
	envelope.RequiredCapability = requestCapability(r)
	if envelope.RequiredCapability == "" {
		switch r.Payload.kind() {
		case SyntheticNoop:
			envelope.RequiredCapability = "synthetic.noop"
		case SyntheticEcho:
			envelope.RequiredCapability = "synthetic.echo"
		}
	}
	data, err := signCommandEnvelope(envelope, signer, candidate)
	return data, payloadType, err
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

func newIDs() (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, error) {
	var ids [5]uuid.UUID
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
