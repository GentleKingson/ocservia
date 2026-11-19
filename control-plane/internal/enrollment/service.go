package enrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/google/uuid"
)

const (
	DefaultTokenTTL      = 15 * time.Minute
	BootstrapTokenPrefix = "obt1_"
	MaxPendingNodes      = 100
	MaxClockSkew         = 5 * time.Minute
	SessionGrantTTL      = 5 * time.Minute
	ProtocolMajor        = 1
	ProtocolMinor        = 1
	MaxMessageSize       = 1 << 20
)

var (
	ErrInvalidToken      = errors.New("enrollment token is invalid or expired")
	ErrEndpointMismatch  = errors.New("endpoint does not match enrollment token")
	ErrEndpointProof     = errors.New("endpoint proof of possession is invalid")
	ErrPendingLimit      = errors.New("pending node limit reached")
	ErrInvalidTransition = errors.New("node state transition is invalid")
	ErrNotFound          = errors.New("resource not found")
	ErrInvalidRequest    = errors.New("enrollment request is invalid")
)

type BootstrapTokenSpec struct {
	ExpectedEndpointID []byte
	WorkspaceID        uuid.UUID
	Environment        string
	ExpectedNodeName   string
	TTL                time.Duration
	ActorID            string
	Reason             string
	RequestID          string
}

type Token struct {
	ID        uuid.UUID
	Value     string
	ExpiresAt time.Time
}

type Revocation struct {
	NodeID                uuid.UUID
	ActorID               string
	Reason                string
	RequestID             string
	IdentityID, SessionID uuid.UUID
}

type NodeTrust struct {
	NodeID     uuid.UUID
	EndpointID []byte
	Revision   uint64
}

type Service struct {
	installation         InstallationTrust
	backend              database.Backend
	now                  func() time.Time
	random               io.Reader
	controllerEndpointID string
	controllerVersion    string
	signer               *commandauth.Signer
	ownerSessions        ownersession.SessionOpener
	publicKeys           userPasswordPublicKeySource
}

func NewBackend(backend database.Backend, controllerEndpointID, controllerVersion string, signer *commandauth.Signer) *Service {
	return &Service{backend: backend, now: time.Now, random: rand.Reader, controllerEndpointID: controllerEndpointID, controllerVersion: controllerVersion, signer: signer}
}

func NewWithOwnerSessionsBackend(backend database.Backend, controllerEndpointID, controllerVersion string, signer *commandauth.Signer, ownerSessions ownersession.SessionOpener) *Service {
	service := NewBackend(backend, controllerEndpointID, controllerVersion, signer)
	service.ownerSessions = ownerSessions
	return service
}

func (s *Service) CreateBootstrapToken(ctx context.Context, spec BootstrapTokenSpec) (Token, error) {
	return s.createBootstrapToken(ctx, spec, "")
}

func (s *Service) createBootstrapToken(ctx context.Context, spec BootstrapTokenSpec, encoded string) (Token, error) {
	if (len(spec.ExpectedEndpointID) != 0 && len(spec.ExpectedEndpointID) != 32) || spec.WorkspaceID == uuid.Nil || !validShort(spec.Environment, 64) || !validOptional(spec.ExpectedNodeName, 128) || !validActor(spec.ActorID, spec.RequestID, spec.Reason) {
		return Token{}, ErrInvalidRequest
	}
	ttl := spec.TTL
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	if ttl <= 0 || ttl > DefaultTokenTTL {
		return Token{}, ErrInvalidRequest
	}
	if encoded == "" {
		raw := make([]byte, 32)
		if _, err := io.ReadFull(s.random, raw); err != nil {
			return Token{}, fmt.Errorf("generate node bootstrap token: %w", err)
		}
		encoded = BootstrapTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	}
	digest := sha256.Sum256([]byte(encoded))
	now := s.now().UTC()
	token := Token{ID: uuid.Must(uuid.NewV7()), Value: encoded, ExpiresAt: now.Add(ttl)}
	at, err := value.FromTime(now)
	if err != nil {
		return Token{}, err
	}
	expires, err := at.Add(ttl)
	if err != nil {
		return Token{}, err
	}
	var expectedName *string
	if spec.ExpectedNodeName != "" {
		expectedName = &spec.ExpectedNodeName
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return Token{}, fmt.Errorf("begin node bootstrap token transaction: %w", err)
	}
	defer rollback(tx)
	workspaceExists, err := store.WorkspaceExists(ctx, spec.WorkspaceID)
	if err != nil {
		return Token{}, fmt.Errorf("check node bootstrap token workspace: %w", err)
	}
	if !workspaceExists {
		return Token{}, ErrNotFound
	}
	err = store.InsertToken(ctx, enrollmentstore.Token{ID: token.ID, WorkspaceID: spec.WorkspaceID, Hash: digest[:], Environment: spec.Environment, ExpectedName: expectedName, ExpectedEndpoint: spec.ExpectedEndpointID, ExpiresAt: expires, CreatedBy: spec.ActorID, CreatedAt: at})
	if err != nil {
		return Token{}, fmt.Errorf("insert node bootstrap token: %w", err)
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: spec.WorkspaceID, ActorType: "user", ActorID: spec.ActorID, Action: "node_bootstrap_token.create", ResourceType: "node_bootstrap_token", ResourceID: token.ID, RequestID: spec.RequestID, Reason: spec.Reason, At: now}); err != nil {
		return Token{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Token{}, fmt.Errorf("commit node bootstrap token transaction: %w", err)
	}
	return token, nil
}

// ValidateEnrollment authenticates the first application message without
// consuming its one-time authority. Enroll repeats these checks while holding
// the token row lock and atomically consumes it with node activation.
func (s *Service) ValidateEnrollment(ctx context.Context, request *agentv1.EnrollRequest) error {
	if err := validateEnrollment(request); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := verifyEnrollmentProof(request); err != nil {
		return err
	}
	digest, ok := bootstrapTokenDigest(request.GetToken())
	if !ok {
		return ErrInvalidToken
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return err
	}
	defer rollback(tx)
	token, err := store.TokenByHash(ctx, digest[:], false)
	if errors.Is(err, database.ErrNotFound) {
		return ErrInvalidToken
	}
	if err != nil {
		return fmt.Errorf("validate enrollment token: %w", err)
	}
	if token.Environment != request.GetEnvironment() || (len(token.ExpectedEndpoint) != 0 && !slices.Equal(token.ExpectedEndpoint, request.GetEndpointId())) {
		return ErrInvalidToken
	}
	if token.ConsumedAt.Valid {
		if token.ConsumedNode == nil || !slices.Equal(token.Endpoint, request.GetEndpointId()) {
			return ErrInvalidToken
		}
		node, err := store.NodeByID(ctx, *token.ConsumedNode, enrollmentstore.ForShare)
		if err != nil || node.WorkspaceID != token.WorkspaceID || !bootstrapNodeState(request.GetToken(), node) || !slices.Equal(node.Endpoint, token.Endpoint) {
			return ErrInvalidToken
		}
		return nil
	}
	if !tokenUnexpired(token.ExpiresAt, s.now()) {
		return ErrInvalidToken
	}
	return nil
}

func (s *Service) Enroll(ctx context.Context, request *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	if err := validateEnrollment(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := verifyEnrollmentProof(request); err != nil {
		return nil, err
	}
	return s.enrollBootstrap(ctx, request)
}

// Recheck expiry after all admission and node/key lock waits. Returning false
// rolls back the node, key and capability writes in the caller's transaction.
func (s *Service) consumeToken(ctx context.Context, store enrollmentstore.EnrollmentStore, token enrollmentstore.Token, node uuid.UUID, endpoint []byte) (bool, error) {
	now := s.now()
	if !tokenUnexpired(token.ExpiresAt, now) {
		return false, nil
	}
	at, err := value.FromTime(now)
	if err != nil {
		return false, err
	}
	return store.ConsumeToken(ctx, token.ID, node, endpoint, at)
}

func bootstrapTokenDigest(value string) ([sha256.Size]byte, bool) {
	if !isBootstrapToken(value) {
		return [sha256.Size]byte{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[5:])
	if err != nil || len(raw) != 32 || value[5:] != base64.RawURLEncoding.EncodeToString(raw) {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256([]byte(value)), true
}

func isBootstrapToken(value string) bool {
	return strings.HasPrefix(value, BootstrapTokenPrefix) || strings.HasPrefix(value, installEnrollmentPrefix)
}

func bootstrapNodeState(_ string, node enrollmentstore.Node) bool {
	return (node.Status == "active" || node.Status == "offline") && node.EndpointState == "active"
}

func (s *Service) enrollBootstrap(ctx context.Context, request *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	digest, ok := bootstrapTokenDigest(request.GetToken())
	if !ok {
		return nil, ErrInvalidToken
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return nil, fmt.Errorf("begin node bootstrap enrollment: %w", err)
	}
	defer rollback(tx)
	token, err := store.TokenByHash(ctx, digest[:], true)
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("lock node bootstrap token: %w", err)
	}
	workspaceID, expectedName := token.WorkspaceID, token.ExpectedName
	if token.Environment != request.GetEnvironment() || (len(token.ExpectedEndpoint) != 0 && !slices.Equal(token.ExpectedEndpoint, request.GetEndpointId())) {
		return nil, ErrInvalidToken
	}
	if token.ConsumedAt.Valid {
		if token.ConsumedNode == nil || len(token.Endpoint) != 32 || subtle.ConstantTimeCompare(token.Endpoint, request.GetEndpointId()) != 1 {
			return nil, ErrEndpointMismatch
		}
		node, err := store.NodeByID(ctx, *token.ConsumedNode, enrollmentstore.ForShare)
		if err != nil || node.WorkspaceID != workspaceID || !bootstrapNodeState(request.GetToken(), node) || subtle.ConstantTimeCompare(node.Endpoint, request.GetEndpointId()) != 1 {
			return nil, ErrInvalidToken
		}
		if match, err := sealingKeysMatch(ctx, store, node.ID, request.GetSealingKeys()); err != nil || !match {
			return nil, ErrEndpointMismatch
		}
		if match, err := supportedCapabilitiesMatch(ctx, store, node.ID, request.GetCapabilities()); err != nil || !match {
			return nil, ErrEndpointMismatch
		}
		return &agentv1.EnrollResponse{Result: bootstrapResult(request.GetToken()), NodeId: (*token.ConsumedNode)[:], ControllerEndpointId: s.controllerEndpointID}, nil
	}
	if !tokenUnexpired(token.ExpiresAt, s.now()) || len(token.Endpoint) != 0 || token.ConsumedNode != nil {
		return nil, ErrInvalidToken
	}
	if err := audit.LockChainTx(ctx, tx, workspaceID); err != nil {
		return nil, err
	}
	endpointExists, err := store.EndpointExists(ctx, request.GetEndpointId())
	if err != nil {
		return nil, fmt.Errorf("check bootstrap endpoint binding: %w", err)
	}
	if endpointExists {
		return nil, ErrEndpointMismatch
	}
	pending, err := store.PendingCount(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("count pending nodes: %w", err)
	}
	if pending >= MaxPendingNodes {
		return nil, ErrPendingLimit
	}
	now := s.now().UTC()
	at, err := value.FromTime(now)
	if err != nil {
		return nil, err
	}
	nodeID := uuid.Must(uuid.NewV7())
	name := "node-" + fmt.Sprintf("%x", request.GetEndpointId()[:6])
	if expectedName != nil {
		name = *expectedName
	}
	if err := store.InsertNode(ctx, nodeID, workspaceID, name, at); err != nil {
		return nil, fmt.Errorf("insert bootstrap node: %w", err)
	}
	if err := store.InsertEndpoint(ctx, nodeID, request.GetEndpointId(), at); err != nil {
		return nil, fmt.Errorf("bind bootstrap endpoint: %w", err)
	}
	sealingKeys := slices.Clone(request.GetSealingKeys())
	slices.SortFunc(sealingKeys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	for _, key := range sealingKeys {
		if err := store.InsertSealingKey(ctx, nodeID, sealingKey(key), at); err != nil {
			return nil, fmt.Errorf("record bootstrap password sealing key: %w", err)
		}
	}
	for _, capability := range normalizedCapabilities(request.GetCapabilities()) {
		if err := store.PutCapability(ctx, nodeID, capability, false); err != nil {
			return nil, fmt.Errorf("record bootstrap requested capability: %w", err)
		}
	}
	consumed, err := s.consumeToken(ctx, store, token, nodeID, request.GetEndpointId())
	if err != nil {
		return nil, fmt.Errorf("consume node bootstrap token: %w", err)
	}
	if !consumed {
		return nil, ErrInvalidToken
	}
	revision, err := store.Activate(ctx, nodeID, "{}", "default", at)
	if err != nil {
		return nil, err
	}
	for _, capability := range normalizedCapabilities(request.GetCapabilities()) {
		if capability == ownersession.FencingCapability || capability == "privd_result_attestation_v1" {
			continue
		}
		if err := store.PutCapability(ctx, nodeID, capability, true); err != nil {
			return nil, err
		}
	}
	activationReason := "one-time bootstrap token"
	if strings.HasPrefix(request.GetToken(), installEnrollmentPrefix) {
		activationReason = "one-time installation token"
	}
	if err := enqueueTrustConvergence(ctx, tx, nodeID, request.GetEndpointId(), "active", revision, activationReason, now); err != nil {
		return nil, err
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "bootstrap", ActorID: token.ID.String(), Action: "node.activate", ResourceType: "node", ResourceID: nodeID, RequestID: uuid.Must(uuid.NewV7()).String(), Reason: activationReason, At: now}); err != nil {
		return nil, err
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: fmt.Sprintf("endpoint:%x", request.GetEndpointId()), Action: "node.enroll", ResourceType: "node", ResourceID: nodeID, RequestID: uuid.Must(uuid.NewV7()).String(), At: now}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit node bootstrap enrollment: %w", err)
	}
	return &agentv1.EnrollResponse{Result: bootstrapResult(request.GetToken()), NodeId: nodeID[:], ControllerEndpointId: s.controllerEndpointID}, nil
}

func (s *Service) Revoke(ctx context.Context, revocation Revocation) (NodeTrust, error) {
	if revocation.NodeID == uuid.Nil || revocation.IdentityID == uuid.Nil || revocation.SessionID == uuid.Nil || !validActor(revocation.ActorID, revocation.RequestID, revocation.Reason) {
		return NodeTrust{}, ErrInvalidRequest
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return NodeTrust{}, fmt.Errorf("begin revocation: %w", err)
	}
	defer rollback(tx)
	node, err := store.NodeByID(ctx, revocation.NodeID, enrollmentstore.ForUpdate)
	if errors.Is(err, database.ErrNotFound) {
		return NodeTrust{}, ErrInvalidTransition
	}
	if err != nil {
		return NodeTrust{}, fmt.Errorf("lock node for revocation: %w", err)
	}
	workspaceID, endpointID, currentStatus, revision := node.WorkspaceID, node.Endpoint, node.Status, node.AuthorizationRevision
	if err := auth.AuthorizeMutationTx(ctx, tx, revocation.IdentityID, revocation.SessionID, workspaceID); err != nil {
		return NodeTrust{}, err
	}
	if currentStatus == "revoked" {
		return NodeTrust{NodeID: revocation.NodeID, EndpointID: endpointID, Revision: revision}, nil
	}
	now := s.now().UTC()
	at, err := value.FromTime(now)
	if err != nil {
		return NodeTrust{}, err
	}
	revision, err = store.Revoke(ctx, revocation.NodeID, at)
	if err != nil {
		return NodeTrust{}, err
	}
	if err := enqueueTrustConvergence(ctx, tx, revocation.NodeID, endpointID, "revoked", revision, revocation.Reason, now); err != nil {
		return NodeTrust{}, err
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: revocation.ActorID, SessionID: &revocation.SessionID, NodeID: &revocation.NodeID, Action: "node.revoke", ResourceType: "node", ResourceID: revocation.NodeID, RequestID: revocation.RequestID, Reason: revocation.Reason, At: now}); err != nil {
		return NodeTrust{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NodeTrust{}, fmt.Errorf("commit revocation: %w", err)
	}
	return NodeTrust{NodeID: revocation.NodeID, EndpointID: endpointID, Revision: revision}, nil
}

// RecordRevocationSynchronized settles the revoked-trust intent Revoke queued
// once the request itself completed the fenced STATE_UPDATE for exactly this
// node, endpoint, revision and reason, and closed reports whether the fenced
// CONNECTION_CLOSE that followed also succeeded. A superseded or different
// intent is left to the convergence worker, which never treats lost
// ownership as delivery.
func (s *Service) RecordRevocationSynchronized(ctx context.Context, trust NodeTrust, reason string, closed bool) (bool, error) {
	var recorded bool
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := enrollmentstore.Trust(tx)
		if err != nil {
			return err
		}
		recorded, err = store.RecordSynchronized(ctx, enrollmentstore.TrustJob{NodeID: trust.NodeID, EndpointID: trust.EndpointID, DesiredState: "revoked", Revision: trust.Revision, Reason: reason}, closed)
		return err
	})
	return recorded, err
}

func (s *Service) CheckEndpoint(ctx context.Context, request *transportv1.CheckEndpointRequest) (bool, error) {
	if len(request.GetEndpointId()) != 32 {
		return false, nil
	}
	enroll := request.GetAlpn() == "ocserv-platform/enroll/1"
	if !enroll && request.GetAlpn() != "ocserv-platform/agent/1" {
		return false, nil
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	return store.EndpointPermitted(ctx, request.GetEndpointId(), enroll)
}

func (s *Service) ListNodeTrust(ctx context.Context) ([]*transportv1.NodeTrustBinding, error) {
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	nodes, err := store.TrustSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("list node trust snapshot: %w", err)
	}
	bindings := make([]*transportv1.NodeTrustBinding, 0, len(nodes))
	for _, node := range nodes {
		trustState := transportv1.NodeTrustState_NODE_TRUST_STATE_ACTIVE
		if node.Status == "revoked" {
			trustState = transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED
		}
		bindings = append(bindings, &transportv1.NodeTrustBinding{NodeId: node.ID[:], EndpointId: node.Endpoint, State: trustState, Revision: node.AuthorizationRevision})
	}
	return bindings, nil
}

func (s *Service) AuthorizeSession(ctx context.Context, request *transportv1.AuthorizeSessionRequest) (*agentv1.SessionHandshakeResponse, error) {
	handshake := request.GetHandshake()
	response := &agentv1.SessionHandshakeResponse{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, MaxMessageSize: MaxMessageSize, ControllerVersion: s.controllerVersion}
	if handshake == nil || len(request.GetRemoteEndpointId()) != 32 || subtle.ConstantTimeCompare(request.GetRemoteEndpointId(), handshake.GetEndpointId()) != 1 {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	// Phase 1: short read-only validation transaction. Releasing this DB
	// connection before OpenSession prevents a two-connection pool deadlock
	// when concurrent agent handshakes acquire owner leases and call transportd.
	var (
		nodeID                uuid.UUID
		authorizationRevision uint64
		negotiated            []string
		fencingNegotiated     bool
		earlyResult           agentv1.HandshakeResult
	)
	if err := func() error {
		tx, store, err := s.begin(ctx, database.ReadCommitted)
		if err != nil {
			return fmt.Errorf("begin session authorization: %w", err)
		}
		defer rollback(tx)
		node, err := store.NodeByEndpoint(ctx, request.GetRemoteEndpointId(), uuid.Nil, enrollmentstore.Unlocked)
		if errors.Is(err, database.ErrNotFound) {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
			return nil
		}
		if err != nil {
			return fmt.Errorf("authorize endpoint: %w", err)
		}
		nodeID = node.ID
		status, endpointState := node.Status, node.EndpointState
		authorizationRevision = node.AuthorizationRevision
		if status == "pending" {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL
			return nil
		}
		if (status != "active" && status != "offline") || endpointState != "active" || authorizationRevision == 0 || !slices.Equal(nodeID[:], handshake.GetNodeId()) {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
			return nil
		}
		if handshake.GetProtocolMajor() != ProtocolMajor {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_INCOMPATIBLE_PROTOCOL
			return nil
		}
		if handshake.GetProtocolMinor() > ProtocolMinor {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_UPGRADE_REQUIRED
			return nil
		}
		if handshake.GetTime() == nil || handshake.GetTime().CheckValid() != nil || s.now().Sub(handshake.GetTime().AsTime()).Abs() > MaxClockSkew {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_CLOCK_SKEW
			return nil
		}
		if !validCapabilities(handshake.GetCapabilities()) {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_CAPABILITY_REJECTED
			return nil
		}
		matchingSealingKeys, err := sealingKeysMatch(ctx, store, nodeID, handshake.GetSealingKeys())
		if err != nil {
			return fmt.Errorf("verify session sealing keys: %w", err)
		}
		legacyReadOnlySealingFallback := len(handshake.GetSealingKeys()) == 0
		if !matchingSealingKeys && !legacyReadOnlySealingFallback {
			earlyResult = agentv1.HandshakeResult_HANDSHAKE_RESULT_CAPABILITY_REJECTED
			return nil
		}
		capabilities, err := store.Capabilities(ctx, nodeID, true)
		if err != nil {
			return err
		}
		approved := make(map[string]struct{}, len(capabilities))
		for _, capability := range capabilities {
			approved[capability] = struct{}{}
		}
		advertised := normalizedCapabilities(handshake.GetCapabilities())
		negotiated = make([]string, 0, len(advertised))
		mutationCapable := handshake.GetProtocolMinor() >= ProtocolMinor && !legacyReadOnlySealingFallback
		for _, capability := range advertised {
			// The fencing capability is a protocol-level negotiation, never a
			// per-node business capability: an approval that echoes every
			// advertised capability must not introduce a second copy of it.
			if capability == ownersession.FencingCapability {
				if mutationCapable {
					fencingNegotiated = true
				}
				continue
			}
			if _, ok := approved[capability]; ok && (mutationCapable || legacyReadOnlyCapability(capability)) {
				negotiated = append(negotiated, capability)
			}
		}
		// The fencing capability records only that the endpoint accepts
		// ConnectionFenceV2 proofs on mutation carriers.
		if fencingNegotiated {
			negotiated = append(negotiated, ownersession.FencingCapability)
		}
		slices.Sort(negotiated)
		negotiated = slices.Compact(negotiated)
		return nil
	}(); err != nil {
		return nil, err
	}
	if earlyResult != agentv1.HandshakeResult_HANDSHAKE_RESULT_UNSPECIFIED {
		response.Result = earlyResult
		return response, nil
	}

	response.ProtocolMinor = handshake.GetProtocolMinor()
	response.NegotiatedCapabilities = negotiated
	var openedFence *agentv1.ConnectionFenceV2
	if handshake.GetProtocolMinor() >= ProtocolMinor {
		if s.signer == nil {
			return nil, errors.New("controller session signer is unavailable")
		}
		var fixedNode [16]byte
		var fixedEndpoint [32]byte
		copy(fixedNode[:], nodeID[:])
		copy(fixedEndpoint[:], request.GetRemoteEndpointId())
		now := s.now().UTC()
		var err error
		response.SessionGrant, err = s.signer.IssueSessionGrant(fixedNode, fixedEndpoint, authorizationRevision, negotiated, ProtocolMajor, ProtocolMinor, now, now.Add(SessionGrantTTL))
		if err != nil {
			return nil, fmt.Errorf("issue session grant: %w", err)
		}
		if fencingNegotiated && s.ownerSessions != nil {
			fence, fenceErr := s.ownerSessions.OpenSession(ctx, fixedNode, fixedEndpoint, authorizationRevision, negotiated)
			if errors.Is(fenceErr, ownersession.ErrNotOwner) {
				// A fencing-capable Agent must retry while another term still
				// owns the lease. Accepting an unbounded read-only downgrade here
				// would leave it connected forever after that lease expires, so a
				// replacement Controller could never establish the higher epoch
				// required to recover ambiguous commands.
				return nil, fmt.Errorf("owner lease is not yet available: %w", fenceErr)
			}
			if fenceErr != nil {
				return nil, fmt.Errorf("open owner session: %w", fenceErr)
			}
			response.ConnectionFence = fence
			openedFence = fence
		}
	}

	// Phase 2: re-verify authority under FOR SHARE so any concurrent revocation
	// or capability/revision mutation fails closed and immediately releases the
	// newly opened owner term before a session grant can be returned.
	if openedFence != nil || handshake.GetProtocolMinor() >= ProtocolMinor {
		recheckTx, recheckStore, err := s.begin(ctx, database.ReadCommitted)
		if err != nil {
			if openedFence != nil {
				s.closeOpenedSession(ctx, nodeID, openedFence)
			}
			return nil, fmt.Errorf("begin session authority confirmation: %w", err)
		}
		defer rollback(recheckTx)
		current, err := recheckStore.NodeByEndpoint(ctx, request.GetRemoteEndpointId(), uuid.Nil, enrollmentstore.ForShare)
		if errors.Is(err, database.ErrNotFound) {
			if openedFence != nil {
				s.closeOpenedSession(ctx, nodeID, openedFence)
			}
			response.SessionGrant = nil
			response.ConnectionFence = nil
			response.NegotiatedCapabilities = nil
			response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
			return response, nil
		}
		if err != nil {
			if openedFence != nil {
				s.closeOpenedSession(ctx, nodeID, openedFence)
			}
			return nil, fmt.Errorf("confirm session authority: %w", err)
		}
		if (current.Status != "active" && current.Status != "offline") || current.EndpointState != "active" || current.AuthorizationRevision != authorizationRevision || current.ID != nodeID {
			if openedFence != nil {
				s.closeOpenedSession(ctx, nodeID, openedFence)
			}
			response.SessionGrant = nil
			response.ConnectionFence = nil
			response.NegotiatedCapabilities = nil
			response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
			return response, nil
		}
		if err := recheckTx.Commit(ctx); err != nil {
			// The session was never granted: end the exact owner term so the
			// lease does not keep renewing behind a failed authorization.
			if openedFence != nil {
				s.closeOpenedSession(ctx, nodeID, openedFence)
			}
			return nil, fmt.Errorf("commit session authorization: %w", err)
		}
	}
	response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED
	response.MaxMessageSize = min(handshake.GetMaxMessageSize(), MaxMessageSize)
	return response, nil
}

// closeOpenedSession ends the owner term a handshake opened when the
// authorization it was opened for failed to commit. The cleanup runs on its
// own deadline because the request context is typically the cancelled cause
// of the failure.
func (s *Service) closeOpenedSession(ctx context.Context, nodeID uuid.UUID, fence *agentv1.ConnectionFenceV2) {
	closer, ok := s.ownerSessions.(ownersession.SessionCloser)
	if !ok {
		return
	}
	connectionID, err := fixed16(fence.GetConnectionId())
	if err != nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = closer.CloseSession(cleanupCtx, nodeID, connectionID, int64(fence.GetOwnerEpoch()))
}

func fixed16(value []byte) ([16]byte, error) {
	if len(value) != 16 {
		return [16]byte{}, errors.New("enrollment: value must be 16 bytes")
	}
	var fixed [16]byte
	copy(fixed[:], value)
	return fixed, nil
}

func validateEnrollment(request *agentv1.EnrollRequest) error {
	if request == nil || len(request.GetEndpointId()) != 32 || !validShort(request.GetAgentVersion(), 128) || !validShort(request.GetOsRelease(), 256) ||
		!validShort(request.GetBootId(), 256) || len(request.GetAgentInstanceId()) != 16 || len(request.GetNonce()) < 16 || len(request.GetNonce()) > 64 ||
		request.GetTime() == nil || request.GetTime().CheckValid() != nil || !validShort(request.GetEnvironment(), 64) || len(request.GetCapabilities()) > 128 ||
		request.GetEnrollmentProtocolMajor() != EnrollmentProtocolMajor || request.GetEnrollmentProtocolMinor() != EnrollmentProtocolMinor {
		return errors.New("invalid enrollment request")
	}
	if !validCapabilities(request.GetCapabilities()) {
		return errors.New("invalid enrollment capabilities")
	}
	keys := slices.Clone(request.GetSealingKeys())
	slices.SortFunc(keys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	if err := validateSealingKeys(keys); err != nil {
		return err
	}
	return nil
}

func sealingKeysMatch(ctx context.Context, store enrollmentstore.EnrollmentStore, nodeID uuid.UUID, advertised []*agentv1.SealingKeyDescriptorV1) (bool, error) {
	keys := slices.Clone(advertised)
	slices.SortFunc(keys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	if err := validateSealingKeys(keys); err != nil {
		return false, nil
	}
	stored, err := store.SealingKeys(ctx, nodeID)
	if err != nil {
		return false, err
	}
	if len(stored) != len(keys) {
		return false, nil
	}
	for index, actual := range stored {
		key := keys[index]
		if actual.Purpose != int32(key.GetPurpose()) || actual.Version != int32(key.GetVersion()) || actual.ID != key.GetKeyId() || subtle.ConstantTimeCompare(actual.Digest, key.GetPublicKeySha256()) != 1 {
			return false, nil
		}
	}
	return true, nil
}

func supportedCapabilitiesMatch(ctx context.Context, store enrollmentstore.EnrollmentStore, nodeID uuid.UUID, advertised []string) (bool, error) {
	want := normalizedCapabilities(advertised)
	have, err := store.Capabilities(ctx, nodeID, false)
	return slices.Equal(have, want), err
}

func enqueueTrustConvergence(ctx context.Context, tx database.Tx, nodeID uuid.UUID, endpointID []byte, state string, revision uint64, reason string, now time.Time) error {
	at, err := value.FromTime(now)
	if err != nil {
		return err
	}
	store, err := enrollmentstore.Trust(tx)
	if err != nil {
		return err
	}
	err = store.Enqueue(ctx, enrollmentstore.TrustJob{NodeID: nodeID, EndpointID: endpointID, DesiredState: state, Revision: revision, Reason: reason}, at)
	if err != nil {
		return fmt.Errorf("enqueue node trust convergence: %w", err)
	}
	return nil
}

func normalizedCapabilities(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if validShort(value, 128) && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return result
}

// Keep grantless/legacy sessions within the reviewed Rust contracts::session
// allowlist; a newly advertised .read capability is not implicitly compatible.
func legacyReadOnlyCapability(capability string) bool {
	switch capability {
	case "ocserv.config_fingerprint.read", "ocserv.ip_bans.read", "ocserv.sessions.read",
		"ocserv.status.read", "ocserv.version.read":
		return true
	default:
		return false
	}
}

func validCapabilities(values []string) bool {
	if len(values) == 0 || len(values) > 128 {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != strings.TrimSpace(value) || !validShort(value, 128) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validShort(value string, maximum int) bool {
	trimmed := strings.TrimSpace(value)
	length := utf8.RuneCountInString(value)
	return value == trimmed && length > 0 && length <= maximum
}
func validOptional(value string, maximum int) bool { return value == "" || validShort(value, maximum) }
func validActor(actor, request, reason string) bool {
	return validShort(actor, 256) && validShort(request, 128) && validShort(reason, 1024)
}
func rollback(tx database.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func tokenUnexpired(expires value.Timestamp, now time.Time) bool {
	at, err := value.FromTime(now)
	return err == nil && expires.Valid && expires.Micros > at.Micros
}

func sealingKey(key *agentv1.SealingKeyDescriptorV1) enrollmentstore.SealingKey {
	return enrollmentstore.SealingKey{Purpose: int32(key.GetPurpose()), Version: int32(key.GetVersion()), ID: key.GetKeyId(), Digest: key.GetPublicKeySha256()}
}

func (s *Service) begin(ctx context.Context, isolation database.Isolation) (database.Tx, enrollmentstore.EnrollmentStore, error) {
	tx, err := s.backend.Begin(ctx, isolation)
	if err != nil {
		return nil, nil, err
	}
	store, err := enrollmentstore.Enrollment(tx)
	if err != nil {
		rollback(tx)
		return nil, nil, err
	}
	return tx, store, nil
}
