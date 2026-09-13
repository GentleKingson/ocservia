package enrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	approvalstore "github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
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

type TokenSpec struct {
	WorkspaceID        uuid.UUID
	Environment        string
	ExpectedNodeName   string
	ExpectedEndpointID []byte
	TTL                time.Duration
	ActorID            string
	Reason             string
	RequestID          string
}

type BootstrapTokenSpec struct {
	WorkspaceID      uuid.UUID
	Environment      string
	ExpectedNodeName string
	TTL              time.Duration
	ActorID          string
	Reason           string
	RequestID        string
}

type Token struct {
	ID        uuid.UUID
	Value     string
	ExpiresAt time.Time
}

type Approval struct {
	NodeID                            uuid.UUID
	Labels                            map[string]string
	Policy                            string
	Capabilities                      []string
	ActorID                           string
	Reason                            string
	RequestID                         string
	ApprovalID, IdentityID, SessionID uuid.UUID
}

type Revocation struct {
	NodeID                            uuid.UUID
	ActorID                           string
	Reason                            string
	RequestID                         string
	ApprovalID, IdentityID, SessionID uuid.UUID
}

type NodeTrust struct {
	NodeID     uuid.UUID
	EndpointID []byte
	Revision   uint64
}

type Service struct {
	backend              database.Backend
	now                  func() time.Time
	random               io.Reader
	controllerEndpointID string
	controllerVersion    string
	signer               *commandauth.Signer
	ownerSessions        ownersession.SessionOpener
}

func NewBackend(backend database.Backend, controllerEndpointID, controllerVersion string, signer *commandauth.Signer) *Service {
	return &Service{backend: backend, now: time.Now, random: rand.Reader, controllerEndpointID: controllerEndpointID, controllerVersion: controllerVersion, signer: signer}
}

func NewWithOwnerSessionsBackend(backend database.Backend, controllerEndpointID, controllerVersion string, signer *commandauth.Signer, ownerSessions ownersession.SessionOpener) *Service {
	service := NewBackend(backend, controllerEndpointID, controllerVersion, signer)
	service.ownerSessions = ownerSessions
	return service
}

func (s *Service) CreateToken(ctx context.Context, spec TokenSpec) (Token, error) {
	if spec.WorkspaceID == uuid.Nil || !validShort(spec.Environment, 64) || !validOptional(spec.ExpectedNodeName, 128) ||
		len(spec.ExpectedEndpointID) != 32 || !validActor(spec.ActorID, spec.RequestID, spec.Reason) {
		return Token{}, ErrInvalidRequest
	}
	ttl := spec.TTL
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	if ttl <= 0 || ttl > DefaultTokenTTL {
		return Token{}, ErrInvalidRequest
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return Token{}, fmt.Errorf("generate enrollment token: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256(raw)
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
		return Token{}, fmt.Errorf("begin token transaction: %w", err)
	}
	defer rollback(tx)
	workspaceExists, err := store.WorkspaceExists(ctx, spec.WorkspaceID)
	if err != nil {
		return Token{}, fmt.Errorf("check token workspace: %w", err)
	}
	if !workspaceExists {
		return Token{}, ErrNotFound
	}
	err = store.InsertToken(ctx, enrollmentstore.Token{ID: token.ID, WorkspaceID: spec.WorkspaceID, Hash: digest[:], Environment: spec.Environment, ExpectedName: expectedName, Endpoint: spec.ExpectedEndpointID, ExpiresAt: expires, CreatedBy: spec.ActorID, CreatedAt: at}, false)
	if err != nil {
		return Token{}, fmt.Errorf("insert enrollment token: %w", err)
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: spec.WorkspaceID, ActorType: "user", ActorID: spec.ActorID, Action: "enrollment_token.create", ResourceType: "enrollment_token", ResourceID: token.ID, RequestID: spec.RequestID, Reason: spec.Reason, At: now}); err != nil {
		return Token{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Token{}, fmt.Errorf("commit token transaction: %w", err)
	}
	return token, nil
}

func (s *Service) CreateBootstrapToken(ctx context.Context, spec BootstrapTokenSpec) (Token, error) {
	if spec.WorkspaceID == uuid.Nil || !validShort(spec.Environment, 64) || !validOptional(spec.ExpectedNodeName, 128) || !validActor(spec.ActorID, spec.RequestID, spec.Reason) {
		return Token{}, ErrInvalidRequest
	}
	ttl := spec.TTL
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	if ttl <= 0 || ttl > DefaultTokenTTL {
		return Token{}, ErrInvalidRequest
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return Token{}, fmt.Errorf("generate node bootstrap token: %w", err)
	}
	encoded := BootstrapTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
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
	err = store.InsertToken(ctx, enrollmentstore.Token{ID: token.ID, WorkspaceID: spec.WorkspaceID, Hash: digest[:], Environment: spec.Environment, ExpectedName: expectedName, ExpiresAt: expires, CreatedBy: spec.ActorID, CreatedAt: at}, true)
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
// the token row lock and atomically consumes it with the pending-node write.
func (s *Service) ValidateEnrollment(ctx context.Context, request *agentv1.EnrollRequest) error {
	if err := validateEnrollment(request); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := verifyEnrollmentProof(request); err != nil {
		return err
	}
	bootstrap := strings.HasPrefix(request.GetToken(), BootstrapTokenPrefix)
	var digest [sha256.Size]byte
	if bootstrap {
		var ok bool
		digest, ok = bootstrapTokenDigest(request.GetToken())
		if !ok {
			return ErrInvalidToken
		}
	} else {
		raw, err := base64.RawURLEncoding.DecodeString(request.GetToken())
		if err != nil || len(raw) != 32 {
			return ErrInvalidToken
		}
		digest = sha256.Sum256(raw)
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return err
	}
	defer rollback(tx)
	token, err := store.TokenByHash(ctx, digest[:], bootstrap, false)
	if errors.Is(err, database.ErrNotFound) {
		return ErrInvalidToken
	}
	if err != nil {
		return fmt.Errorf("validate enrollment token: %w", err)
	}
	if token.Environment != request.GetEnvironment() {
		return ErrInvalidToken
	}
	if bootstrap && token.ConsumedAt.Valid {
		if token.ConsumedNode == nil || !slices.Equal(token.Endpoint, request.GetEndpointId()) {
			return ErrInvalidToken
		}
		node, err := store.NodeByID(ctx, *token.ConsumedNode, enrollmentstore.ForShare)
		if err != nil || node.WorkspaceID != token.WorkspaceID || node.Status != "pending" || node.EndpointState != "pending" || !slices.Equal(node.Endpoint, token.Endpoint) {
			return ErrInvalidToken
		}
		return nil
	}
	if token.ConsumedAt.Valid || !tokenUnexpired(token.ExpiresAt, s.now()) || (!bootstrap && !slices.Equal(token.Endpoint, request.GetEndpointId())) {
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
	if strings.HasPrefix(request.GetToken(), BootstrapTokenPrefix) {
		return s.enrollBootstrap(ctx, request)
	}
	raw, err := base64.RawURLEncoding.DecodeString(request.GetToken())
	if err != nil || len(raw) != 32 {
		return nil, ErrInvalidToken
	}
	digest := sha256.Sum256(raw)
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return nil, fmt.Errorf("begin enrollment transaction: %w", err)
	}
	defer rollback(tx)
	token, err := store.TokenByHash(ctx, digest[:], false, true)
	if err := validateLockedToken(err, token.ConsumedAt.Valid, token.ExpiresAt, s.now()); err != nil {
		return nil, err
	}
	workspaceID, expectedName := token.WorkspaceID, token.ExpectedName
	if token.Environment != request.GetEnvironment() {
		return nil, ErrInvalidToken
	}
	if len(token.Endpoint) != 32 || subtle.ConstantTimeCompare(token.Endpoint, request.GetEndpointId()) != 1 {
		return nil, ErrEndpointMismatch
	}
	if err := audit.LockChainTx(ctx, tx, workspaceID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	at, err := value.FromTime(now)
	if err != nil {
		return nil, err
	}
	existing, existingErr := store.NodeByEndpoint(ctx, request.GetEndpointId(), workspaceID, enrollmentstore.ForUpdate)
	if existingErr == nil {
		existingNodeID, existingNodeName, existingNodeStatus, existingEndpointState := existing.ID, existing.Name, existing.Status, existing.EndpointState
		storedKeys, err := store.SealingKeys(ctx, existingNodeID)
		if err != nil {
			return nil, fmt.Errorf("count existing password sealing keys: %w", err)
		}
		capabilitiesMatch, err := supportedCapabilitiesMatch(ctx, store, existingNodeID, request.GetCapabilities())
		if err != nil {
			return nil, fmt.Errorf("verify existing node capabilities: %w", err)
		}
		validState := (existingNodeStatus == "active" || existingNodeStatus == "offline") && existingEndpointState == "active" ||
			existingNodeStatus == "pending" && existingEndpointState == "pending"
		if len(storedKeys) != 0 || !capabilitiesMatch || !validState || expectedName != nil && *expectedName != existingNodeName {
			return nil, ErrInvalidToken
		}
		sealingKeys := slices.Clone(request.GetSealingKeys())
		slices.SortFunc(sealingKeys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
		for _, key := range sealingKeys {
			if err := store.InsertSealingKey(ctx, existingNodeID, sealingKey(key), at); err != nil {
				return nil, fmt.Errorf("bind existing node password sealing key: %w", err)
			}
		}
		consumed, err := s.consumeToken(ctx, store, token, existingNodeID, nil, false)
		if err != nil {
			return nil, fmt.Errorf("consume sealing key enrollment token: %w", err)
		}
		if !consumed {
			return nil, ErrInvalidToken
		}
		if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: fmt.Sprintf("endpoint:%x", request.GetEndpointId()), Action: "node.sealing_keys.bind", ResourceType: "node", ResourceID: existingNodeID, RequestID: uuid.Must(uuid.NewV7()).String(), At: now}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit existing node password sealing keys: %w", err)
		}
		result := agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL
		if existingNodeStatus == "active" || existingNodeStatus == "offline" {
			result = agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED
		}
		return &agentv1.EnrollResponse{Result: result, NodeId: existingNodeID[:], ControllerEndpointId: s.controllerEndpointID}, nil
	}
	if !errors.Is(existingErr, database.ErrNotFound) {
		return nil, fmt.Errorf("lock existing endpoint binding: %w", existingErr)
	}
	name := "node-" + fmt.Sprintf("%x", request.GetEndpointId()[:6])
	var nodeID uuid.UUID
	claimedLegacyNode := false
	if expectedName != nil {
		name = *expectedName
		var err error
		nodeID, err = store.LegacyPendingNode(ctx, workspaceID, name)
		if err == nil {
			claimedLegacyNode = true
		} else if !errors.Is(err, database.ErrNotFound) {
			return nil, fmt.Errorf("lock legacy pending node: %w", err)
		}
	}
	if !claimedLegacyNode {
		pending, err := store.PendingCount(ctx, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("count pending nodes: %w", err)
		}
		if pending >= MaxPendingNodes {
			return nil, ErrPendingLimit
		}
		nodeID = uuid.Must(uuid.NewV7())
		if err := store.InsertNode(ctx, nodeID, workspaceID, name, at); err != nil {
			return nil, fmt.Errorf("insert pending node: %w", err)
		}
	} else if err := store.TouchNode(ctx, nodeID, at); err != nil {
		return nil, fmt.Errorf("prepare legacy pending node: %w", err)
	}
	if err := store.InsertEndpoint(ctx, nodeID, request.GetEndpointId(), at); err != nil {
		return nil, fmt.Errorf("bind pending endpoint: %w", err)
	}
	sealingKeys := slices.Clone(request.GetSealingKeys())
	slices.SortFunc(sealingKeys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	for _, key := range sealingKeys {
		if err := store.InsertSealingKey(ctx, nodeID, sealingKey(key), at); err != nil {
			return nil, fmt.Errorf("record password sealing key: %w", err)
		}
	}
	for _, capability := range normalizedCapabilities(request.GetCapabilities()) {
		if err := store.PutCapability(ctx, nodeID, capability, false); err != nil {
			return nil, fmt.Errorf("record requested capability: %w", err)
		}
	}
	consumed, err := s.consumeToken(ctx, store, token, nodeID, nil, false)
	if err != nil {
		return nil, fmt.Errorf("consume enrollment token: %w", err)
	}
	if !consumed {
		return nil, ErrInvalidToken
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: fmt.Sprintf("endpoint:%x", request.GetEndpointId()), Action: "node.enroll", ResourceType: "node", ResourceID: nodeID, RequestID: uuid.Must(uuid.NewV7()).String(), At: now}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit enrollment: %w", err)
	}
	return &agentv1.EnrollResponse{Result: agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL, NodeId: nodeID[:], ControllerEndpointId: s.controllerEndpointID}, nil
}

// Recheck expiry after all admission and node/key lock waits. Returning false
// rolls back the node, key and capability writes in the caller's transaction.
func (s *Service) consumeToken(ctx context.Context, store enrollmentstore.EnrollmentStore, token enrollmentstore.Token, node uuid.UUID, endpoint []byte, bootstrap bool) (bool, error) {
	now := s.now()
	if !tokenUnexpired(token.ExpiresAt, now) {
		return false, nil
	}
	at, err := value.FromTime(now)
	if err != nil {
		return false, err
	}
	return store.ConsumeToken(ctx, token.ID, node, endpoint, at, bootstrap)
}

func bootstrapTokenDigest(value string) ([sha256.Size]byte, bool) {
	var zero [sha256.Size]byte
	if !strings.HasPrefix(value, BootstrapTokenPrefix) {
		return zero, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, BootstrapTokenPrefix))
	if err != nil || len(raw) != 32 || value != BootstrapTokenPrefix+base64.RawURLEncoding.EncodeToString(raw) {
		return zero, false
	}
	return sha256.Sum256([]byte(value)), true
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
	token, err := store.TokenByHash(ctx, digest[:], true, true)
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("lock node bootstrap token: %w", err)
	}
	workspaceID, expectedName := token.WorkspaceID, token.ExpectedName
	if token.Environment != request.GetEnvironment() {
		return nil, ErrInvalidToken
	}
	if token.ConsumedAt.Valid {
		if token.ConsumedNode == nil || len(token.Endpoint) != 32 || subtle.ConstantTimeCompare(token.Endpoint, request.GetEndpointId()) != 1 {
			return nil, ErrEndpointMismatch
		}
		node, err := store.NodeByID(ctx, *token.ConsumedNode, enrollmentstore.ForShare)
		if err != nil || node.WorkspaceID != workspaceID || node.Status != "pending" || node.EndpointState != "pending" || subtle.ConstantTimeCompare(node.Endpoint, request.GetEndpointId()) != 1 {
			return nil, ErrInvalidToken
		}
		return &agentv1.EnrollResponse{Result: agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL, NodeId: (*token.ConsumedNode)[:], ControllerEndpointId: s.controllerEndpointID}, nil
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
		return nil, fmt.Errorf("insert bootstrap pending node: %w", err)
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
	consumed, err := s.consumeToken(ctx, store, token, nodeID, request.GetEndpointId(), true)
	if err != nil {
		return nil, fmt.Errorf("consume node bootstrap token: %w", err)
	}
	if !consumed {
		return nil, ErrInvalidToken
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: fmt.Sprintf("endpoint:%x", request.GetEndpointId()), Action: "node.enroll", ResourceType: "node", ResourceID: nodeID, RequestID: uuid.Must(uuid.NewV7()).String(), At: now}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit node bootstrap enrollment: %w", err)
	}
	return &agentv1.EnrollResponse{Result: agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL, NodeId: nodeID[:], ControllerEndpointId: s.controllerEndpointID}, nil
}

func (s *Service) Approve(ctx context.Context, approval Approval) (NodeTrust, error) {
	if approval.NodeID == uuid.Nil || approval.ApprovalID == uuid.Nil || approval.IdentityID == uuid.Nil || approval.SessionID == uuid.Nil || !validActor(approval.ActorID, approval.RequestID, approval.Reason) || !validPolicy(approval.Policy) || len(approval.Labels) > 32 {
		return NodeTrust{}, ErrInvalidRequest
	}
	if !validCapabilities(approval.Capabilities) {
		return NodeTrust{}, ErrInvalidRequest
	}
	for key, value := range approval.Labels {
		if !validShort(key, 64) || !validShort(value, 128) {
			return NodeTrust{}, ErrInvalidRequest
		}
	}
	capabilities := normalizedCapabilities(approval.Capabilities)
	if len(capabilities) == 0 {
		return NodeTrust{}, ErrInvalidRequest
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return NodeTrust{}, fmt.Errorf("begin approval: %w", err)
	}
	defer rollback(tx)
	node, err := store.NodeByID(ctx, approval.NodeID, enrollmentstore.ForUpdate)
	if errors.Is(err, database.ErrNotFound) {
		return NodeTrust{}, ErrInvalidTransition
	}
	if err != nil {
		return NodeTrust{}, fmt.Errorf("lock pending node: %w", err)
	}
	workspaceID, endpointID, currentStatus, revision, nodeVersion := node.WorkspaceID, node.Endpoint, node.Status, node.AuthorizationRevision, node.Version
	if currentStatus != "pending" && currentStatus != "active" && currentStatus != "offline" {
		return NodeTrust{}, ErrInvalidTransition
	}
	if currentStatus == "active" || currentStatus == "offline" {
		// Activation increments the node version once. Reconstruct the original
		// approved content so an idempotent retry cannot substitute policy,
		// labels, or capabilities after approval.
		if nodeVersion < 2 {
			return NodeTrust{}, ErrInvalidTransition
		}
		requestHash, _, bindingErr := nodeApprovalBinding(ctx, store, approval.NodeID, endpointID, nodeVersion-1, approval.Labels, approval.Policy, capabilities)
		if bindingErr != nil {
			return NodeTrust{}, bindingErr
		}
		if err := approvalstore.ValidateConsumedBoundTx(ctx, tx, approval.ApprovalID, workspaceID, approval.IdentityID, "node.approve", "node", approval.NodeID, requestHash); err != nil {
			return NodeTrust{}, err
		}
		return NodeTrust{NodeID: approval.NodeID, EndpointID: endpointID, Revision: revision}, nil
	}
	requestHash, _, err := nodeApprovalBinding(ctx, store, approval.NodeID, endpointID, nodeVersion, approval.Labels, approval.Policy, capabilities)
	if err != nil {
		return NodeTrust{}, err
	}
	if err := approvalstore.ConsumeBoundTx(ctx, tx, approval.ApprovalID, workspaceID, approval.IdentityID, "node.approve", "node", approval.NodeID, requestHash); err != nil {
		return NodeTrust{}, err
	}
	labels := mapToJSON(approval.Labels)
	now := s.now().UTC()
	at, err := value.FromTime(now)
	if err != nil {
		return NodeTrust{}, err
	}
	revision, err = store.Activate(ctx, approval.NodeID, labels, approval.Policy, at)
	if err != nil {
		return NodeTrust{}, fmt.Errorf("activate node and endpoint: %w", err)
	}
	if err := enqueueTrustConvergence(ctx, tx, approval.NodeID, endpointID, "active", revision, approval.Reason, now); err != nil {
		return NodeTrust{}, err
	}
	if err := store.ResetCapabilities(ctx, approval.NodeID); err != nil {
		return NodeTrust{}, err
	}
	for _, capability := range capabilities {
		// Protocol capabilities are negotiated, never business-approved: the
		// fencing capability only records that the endpoint accepts fences,
		// and an approval echoing every advertised capability must not turn
		// it into an approved node capability.
		if capability == ownersession.FencingCapability {
			continue
		}
		if err := store.PutCapability(ctx, approval.NodeID, capability, true); err != nil {
			return NodeTrust{}, fmt.Errorf("approve capability: %w", err)
		}
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: approval.ActorID, SessionID: &approval.SessionID, ApprovalID: &approval.ApprovalID, NodeID: &approval.NodeID, Action: "node.approve", ResourceType: "node", ResourceID: approval.NodeID, RequestID: approval.RequestID, Reason: approval.Reason, At: now}); err != nil {
		return NodeTrust{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NodeTrust{}, fmt.Errorf("commit approval: %w", err)
	}
	return NodeTrust{NodeID: approval.NodeID, EndpointID: endpointID, Revision: revision}, nil
}

func (s *Service) ApprovalBinding(ctx context.Context, nodeID uuid.UUID, labels map[string]string, policy string, capabilities []string) (uuid.UUID, []byte, json.RawMessage, error) {
	capabilities = normalizedCapabilities(capabilities)
	if nodeID == uuid.Nil || !validPolicy(policy) || len(labels) > 32 || !validCapabilities(capabilities) || len(capabilities) == 0 {
		return uuid.Nil, nil, nil, ErrInvalidRequest
	}
	for key, value := range labels {
		if !validShort(key, 64) || !validShort(value, 128) {
			return uuid.Nil, nil, nil, ErrInvalidRequest
		}
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return uuid.Nil, nil, nil, err
	}
	defer rollback(tx)
	node, err := store.NodeByID(ctx, nodeID, enrollmentstore.Unlocked)
	if err != nil {
		return uuid.Nil, nil, nil, err
	}
	if node.Status != "pending" || node.EndpointState != "pending" {
		return uuid.Nil, nil, nil, database.ErrNotFound
	}
	hash, summary, err := nodeApprovalBinding(ctx, store, nodeID, node.Endpoint, node.Version, labels, policy, capabilities)
	return node.WorkspaceID, hash, summary, err
}

func nodeApprovalBinding(ctx context.Context, store enrollmentstore.EnrollmentStore, nodeID uuid.UUID, endpointID []byte, version int64, labels map[string]string, policy string, capabilities []string) ([]byte, json.RawMessage, error) {
	supported, err := store.Capabilities(ctx, nodeID, false)
	if err != nil {
		return nil, nil, err
	}
	for _, capability := range capabilities {
		if !slices.Contains(supported, capability) {
			return nil, nil, ErrInvalidRequest
		}
	}
	labelKeys := make([]string, 0, len(labels))
	for key := range labels {
		labelKeys = append(labelKeys, key)
	}
	slices.Sort(labelKeys)
	orderedLabels := make([][2]string, 0, len(labelKeys))
	for _, key := range labelKeys {
		orderedLabels = append(orderedLabels, [2]string{key, labels[key]})
	}
	capabilities = append([]string(nil), capabilities...)
	slices.Sort(capabilities)
	summary, _ := json.Marshal(struct {
		NodeID       uuid.UUID   `json:"node_id"`
		EndpointID   string      `json:"endpoint_id"`
		NodeVersion  int64       `json:"node_version"`
		Policy       string      `json:"policy"`
		Labels       [][2]string `json:"labels"`
		Capabilities []string    `json:"capabilities"`
	}{nodeID, fmt.Sprintf("%x", endpointID), version, policy, orderedLabels, capabilities})
	digest := sha256.Sum256(append([]byte("ocservia/node-approval/v1\x00"), summary...))
	return digest[:], summary, nil
}

func (s *Service) Revoke(ctx context.Context, revocation Revocation) (NodeTrust, error) {
	if revocation.NodeID == uuid.Nil || revocation.ApprovalID == uuid.Nil || revocation.IdentityID == uuid.Nil || revocation.SessionID == uuid.Nil || !validActor(revocation.ActorID, revocation.RequestID, revocation.Reason) {
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
	revokeHash, _ := approvalstore.GenericBinding("node.revoke", "node", revocation.NodeID)
	if currentStatus == "revoked" {
		if err := approvalstore.ValidateConsumedBoundTx(ctx, tx, revocation.ApprovalID, workspaceID, revocation.IdentityID, "node.revoke", "node", revocation.NodeID, revokeHash); err != nil {
			return NodeTrust{}, err
		}
		return NodeTrust{NodeID: revocation.NodeID, EndpointID: endpointID, Revision: revision}, nil
	}
	if err := approvalstore.ConsumeBoundTx(ctx, tx, revocation.ApprovalID, workspaceID, revocation.IdentityID, "node.revoke", "node", revocation.NodeID, revokeHash); err != nil {
		return NodeTrust{}, err
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
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: revocation.ActorID, SessionID: &revocation.SessionID, ApprovalID: &revocation.ApprovalID, NodeID: &revocation.NodeID, Action: "node.revoke", ResourceType: "node", ResourceID: revocation.NodeID, RequestID: revocation.RequestID, Reason: revocation.Reason, At: now}); err != nil {
		return NodeTrust{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NodeTrust{}, fmt.Errorf("commit revocation: %w", err)
	}
	return NodeTrust{NodeID: revocation.NodeID, EndpointID: endpointID, Revision: revision}, nil
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
	node, err := s.nodeByEndpoint(ctx, request.GetRemoteEndpointId())
	if errors.Is(err, database.ErrNotFound) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authorize endpoint: %w", err)
	}
	nodeID, status, endpointState := node.ID, node.Status, node.EndpointState
	if status == "pending" {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL
		return response, nil
	}
	if (status != "active" && status != "offline") || endpointState != "active" || !slices.Equal(nodeID[:], handshake.GetNodeId()) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	if handshake.GetProtocolMajor() != ProtocolMajor {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_INCOMPATIBLE_PROTOCOL
		return response, nil
	}
	if handshake.GetProtocolMinor() > ProtocolMinor {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_UPGRADE_REQUIRED
		return response, nil
	}
	if handshake.GetTime() == nil || handshake.GetTime().CheckValid() != nil || s.now().Sub(handshake.GetTime().AsTime()).Abs() > MaxClockSkew {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_CLOCK_SKEW
		return response, nil
	}
	if !validCapabilities(handshake.GetCapabilities()) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_CAPABILITY_REJECTED
		return response, nil
	}
	tx, store, err := s.begin(ctx, database.RepeatableRead)
	if err != nil {
		return nil, fmt.Errorf("begin session authorization: %w", err)
	}
	defer rollback(tx)
	node, err = store.NodeByEndpoint(ctx, request.GetRemoteEndpointId(), uuid.Nil, enrollmentstore.ForShare)
	if errors.Is(err, database.ErrNotFound) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock session authority: %w", err)
	}
	nodeID, status, endpointState = node.ID, node.Status, node.EndpointState
	authorizationRevision := node.AuthorizationRevision
	if (status != "active" && status != "offline") || endpointState != "active" || authorizationRevision == 0 || !slices.Equal(nodeID[:], handshake.GetNodeId()) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	matchingSealingKeys, err := sealingKeysMatch(ctx, store, nodeID, handshake.GetSealingKeys())
	if err != nil {
		return nil, fmt.Errorf("verify session sealing keys: %w", err)
	}
	legacyReadOnlySealingFallback := len(handshake.GetSealingKeys()) == 0
	if !matchingSealingKeys && !legacyReadOnlySealingFallback {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_CAPABILITY_REJECTED
		return response, nil
	}
	capabilities, err := store.Capabilities(ctx, nodeID, true)
	if err != nil {
		return nil, err
	}
	approved := map[string]struct{}{}
	for _, capability := range capabilities {
		approved[capability] = struct{}{}
	}
	negotiated := make([]string, 0, len(handshake.GetCapabilities()))
	for _, capability := range normalizedCapabilities(handshake.GetCapabilities()) {
		// The fencing capability is a protocol-level negotiation, never a
		// per-node business capability: an approval that echoes every
		// advertised capability must not introduce a second copy of it.
		if capability == ownersession.FencingCapability {
			continue
		}
		mutationCapable := handshake.GetProtocolMinor() >= ProtocolMinor && !legacyReadOnlySealingFallback
		if _, ok := approved[capability]; ok && (mutationCapable || strings.HasSuffix(capability, ".read")) {
			negotiated = append(negotiated, capability)
		}
	}
	// The fencing capability records only that the endpoint accepts
	// ConnectionFenceV2 proofs on mutation carriers.
	fencingNegotiated := handshake.GetProtocolMinor() >= ProtocolMinor && !legacyReadOnlySealingFallback &&
		slices.Contains(normalizedCapabilities(handshake.GetCapabilities()), ownersession.FencingCapability)
	if fencingNegotiated {
		negotiated = append(negotiated, ownersession.FencingCapability)
	}
	slices.Sort(negotiated)
	negotiated = slices.Compact(negotiated)
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
	response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED
	response.MaxMessageSize = min(handshake.GetMaxMessageSize(), MaxMessageSize)
	if err := tx.Commit(ctx); err != nil {
		// The session was never granted: end the exact owner term so the
		// lease does not keep renewing behind a failed authorization.
		if openedFence != nil {
			s.closeOpenedSession(ctx, nodeID, openedFence)
		}
		return nil, fmt.Errorf("commit session authorization: %w", err)
	}
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
func validPolicy(value string) bool                { return validShort(value, 128) }
func validActor(actor, request, reason string) bool {
	return validShort(actor, 256) && validShort(request, 128) && validShort(reason, 1024)
}
func validateLockedToken(queryErr error, consumed bool, expiresAt value.Timestamp, now time.Time) error {
	if errors.Is(queryErr, database.ErrNotFound) {
		return ErrInvalidToken
	}
	if queryErr != nil {
		return fmt.Errorf("lock enrollment token: %w", queryErr)
	}
	if consumed || !tokenUnexpired(expiresAt, now) {
		return ErrInvalidToken
	}
	return nil
}
func mapToJSON(values map[string]string) string {
	data, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(data)
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

func (s *Service) nodeByEndpoint(ctx context.Context, endpoint []byte) (enrollmentstore.Node, error) {
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return enrollmentstore.Node{}, err
	}
	defer rollback(tx)
	return store.NodeByEndpoint(ctx, endpoint, uuid.Nil, enrollmentstore.Unlocked)
}
