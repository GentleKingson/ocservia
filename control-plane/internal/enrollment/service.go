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
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	pool                 *pgxpool.Pool
	now                  func() time.Time
	random               io.Reader
	controllerEndpointID string
	controllerVersion    string
	signer               *commandauth.Signer
	ownerSessions        ownersession.SessionOpener
}

func New(pool *pgxpool.Pool, controllerEndpointID, controllerVersion string, signer *commandauth.Signer) *Service {
	return &Service{pool: pool, now: time.Now, random: rand.Reader, controllerEndpointID: controllerEndpointID, controllerVersion: controllerVersion, signer: signer}
}

// NewWithOwnerSessions additionally binds the per-node connection owner
// authority: mutation-capable sessions of fence-capable agents receive a
// Controller-signed owner fence bound to the current ownership term.
func NewWithOwnerSessions(pool *pgxpool.Pool, controllerEndpointID, controllerVersion string, signer *commandauth.Signer, ownerSessions ownersession.SessionOpener) *Service {
	service := New(pool, controllerEndpointID, controllerVersion, signer)
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
	value := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256(raw)
	now := s.now().UTC()
	token := Token{ID: uuid.Must(uuid.NewV7()), Value: value, ExpiresAt: now.Add(ttl)}
	var expectedName any
	if spec.ExpectedNodeName != "" {
		expectedName = spec.ExpectedNodeName
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Token{}, fmt.Errorf("begin token transaction: %w", err)
	}
	defer rollback(tx)
	var workspaceExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, spec.WorkspaceID).Scan(&workspaceExists); err != nil {
		return Token{}, fmt.Errorf("check token workspace: %w", err)
	}
	if !workspaceExists {
		return Token{}, ErrNotFound
	}
	_, err = tx.Exec(ctx, `INSERT INTO enrollment_tokens
        (id, workspace_id, token_hash, expected_environment, expected_node_name, expected_endpoint_id, expires_at, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		token.ID, spec.WorkspaceID, digest[:], spec.Environment, expectedName, spec.ExpectedEndpointID, token.ExpiresAt, spec.ActorID, now)
	if err != nil {
		return Token{}, fmt.Errorf("insert enrollment token: %w", err)
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: spec.WorkspaceID, ActorType: "user", ActorID: spec.ActorID, Action: "enrollment_token.create", ResourceType: "enrollment_token", ResourceID: token.ID, RequestID: spec.RequestID, Reason: spec.Reason, At: now}); err != nil {
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
	value := BootstrapTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(value))
	now := s.now().UTC()
	token := Token{ID: uuid.Must(uuid.NewV7()), Value: value, ExpiresAt: now.Add(ttl)}
	var expectedName any
	if spec.ExpectedNodeName != "" {
		expectedName = spec.ExpectedNodeName
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Token{}, fmt.Errorf("begin node bootstrap token transaction: %w", err)
	}
	defer rollback(tx)
	var workspaceExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, spec.WorkspaceID).Scan(&workspaceExists); err != nil {
		return Token{}, fmt.Errorf("check node bootstrap token workspace: %w", err)
	}
	if !workspaceExists {
		return Token{}, ErrNotFound
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_bootstrap_tokens
		(id,workspace_id,token_hash,expected_environment,expected_node_name,expires_at,created_by,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, token.ID, spec.WorkspaceID, digest[:], spec.Environment, expectedName, token.ExpiresAt, spec.ActorID, now)
	if err != nil {
		return Token{}, fmt.Errorf("insert node bootstrap token: %w", err)
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: spec.WorkspaceID, ActorType: "user", ActorID: spec.ActorID, Action: "node_bootstrap_token.create", ResourceType: "node_bootstrap_token", ResourceID: token.ID, RequestID: spec.RequestID, Reason: spec.Reason, At: now}); err != nil {
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
	if strings.HasPrefix(request.GetToken(), BootstrapTokenPrefix) {
		digest, ok := bootstrapTokenDigest(request.GetToken())
		if !ok {
			return ErrInvalidToken
		}
		var permitted bool
		err := s.pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM node_bootstrap_tokens
			WHERE token_hash=$1 AND expected_environment=$2
			  AND ((consumed_at IS NULL AND expires_at>$3) OR
			       (consumed_at IS NOT NULL AND bound_endpoint_id=$4)))`,
			digest[:], request.GetEnvironment(), s.now(), request.GetEndpointId()).Scan(&permitted)
		if err != nil {
			return fmt.Errorf("validate node bootstrap token: %w", err)
		}
		if !permitted {
			return ErrInvalidToken
		}
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(request.GetToken())
	if err != nil || len(raw) != 32 {
		return ErrInvalidToken
	}
	digest := sha256.Sum256(raw)
	var permitted bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM enrollment_tokens
		WHERE token_hash=$1 AND expected_environment=$2 AND expected_endpoint_id=$3
		  AND consumed_at IS NULL AND expires_at>$4)`, digest[:], request.GetEnvironment(), request.GetEndpointId(), s.now()).Scan(&permitted)
	if err != nil {
		return fmt.Errorf("validate enrollment token: %w", err)
	}
	if !permitted {
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin enrollment transaction: %w", err)
	}
	defer rollback(tx)
	var tokenID, workspaceID uuid.UUID
	var environment string
	var expectedName *string
	var expectedEndpoint []byte
	var expiresAt time.Time
	var consumedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT id, workspace_id, expected_environment, expected_node_name,
	        expected_endpoint_id, expires_at, consumed_at FROM enrollment_tokens WHERE token_hash=$1 FOR UPDATE`, digest[:]).
		Scan(&tokenID, &workspaceID, &environment, &expectedName, &expectedEndpoint, &expiresAt, &consumedAt)
	if err := validateLockedToken(err, consumedAt != nil, expiresAt, s.now()); err != nil {
		return nil, err
	}
	if environment != request.GetEnvironment() {
		return nil, ErrInvalidToken
	}
	if len(expectedEndpoint) != 32 || subtle.ConstantTimeCompare(expectedEndpoint, request.GetEndpointId()) != 1 {
		return nil, ErrEndpointMismatch
	}
	if err := audit.LockChain(ctx, tx, workspaceID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	var existingNodeID uuid.UUID
	var existingNodeName, existingNodeStatus, existingEndpointState string
	existingErr := tx.QueryRow(ctx, `SELECT n.id,n.name,n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id
		WHERE n.workspace_id=$1 AND k.endpoint_id=$2 FOR UPDATE OF n,k`, workspaceID, request.GetEndpointId()).
		Scan(&existingNodeID, &existingNodeName, &existingNodeStatus, &existingEndpointState)
	if existingErr == nil {
		var sealingKeyCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM node_sealing_keys WHERE node_id=$1`, existingNodeID).Scan(&sealingKeyCount); err != nil {
			return nil, fmt.Errorf("count existing password sealing keys: %w", err)
		}
		capabilitiesMatch, err := supportedCapabilitiesMatch(ctx, tx, existingNodeID, request.GetCapabilities())
		if err != nil {
			return nil, fmt.Errorf("verify existing node capabilities: %w", err)
		}
		validState := (existingNodeStatus == "active" || existingNodeStatus == "offline") && existingEndpointState == "active" ||
			existingNodeStatus == "pending" && existingEndpointState == "pending"
		if sealingKeyCount != 0 || !capabilitiesMatch || !validState || expectedName != nil && *expectedName != existingNodeName {
			return nil, ErrInvalidToken
		}
		sealingKeys := slices.Clone(request.GetSealingKeys())
		slices.SortFunc(sealingKeys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
		for _, key := range sealingKeys {
			if _, err := tx.Exec(ctx, `INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6)`, existingNodeID, key.GetPurpose(), key.GetVersion(), key.GetKeyId(), key.GetPublicKeySha256(), now); err != nil {
				return nil, fmt.Errorf("bind existing node password sealing key: %w", err)
			}
		}
		command, err := tx.Exec(ctx, `UPDATE enrollment_tokens SET consumed_at=$1,consumed_node_id=$2 WHERE id=$3 AND consumed_at IS NULL`, now, existingNodeID, tokenID)
		if err != nil {
			return nil, fmt.Errorf("consume sealing key enrollment token: %w", err)
		}
		if command.RowsAffected() != 1 {
			return nil, ErrInvalidToken
		}
		if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: fmt.Sprintf("endpoint:%x", request.GetEndpointId()), Action: "node.sealing_keys.bind", ResourceType: "node", ResourceID: existingNodeID, RequestID: uuid.Must(uuid.NewV7()).String(), At: now}); err != nil {
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
	if !errors.Is(existingErr, pgx.ErrNoRows) {
		return nil, fmt.Errorf("lock existing endpoint binding: %w", existingErr)
	}
	name := "node-" + fmt.Sprintf("%x", request.GetEndpointId()[:6])
	var nodeID uuid.UUID
	claimedLegacyNode := false
	if expectedName != nil {
		name = *expectedName
		err := tx.QueryRow(ctx, `SELECT n.id FROM nodes n
			WHERE n.workspace_id=$1 AND n.name=$2 AND n.status='pending'
			AND NOT EXISTS (SELECT 1 FROM node_endpoint_keys k WHERE k.node_id=n.id)
			FOR UPDATE OF n`, workspaceID, name).Scan(&nodeID)
		if err == nil {
			claimedLegacyNode = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lock legacy pending node: %w", err)
		}
	}
	if !claimedLegacyNode {
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE workspace_id=$1 AND status='pending'`, workspaceID).Scan(&pending); err != nil {
			return nil, fmt.Errorf("count pending nodes: %w", err)
		}
		if pending >= MaxPendingNodes {
			return nil, ErrPendingLimit
		}
		nodeID = uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ($1,$2,$3,'pending',$4,$4)`, nodeID, workspaceID, name, now); err != nil {
			return nil, fmt.Errorf("insert pending node: %w", err)
		}
	} else if _, err := tx.Exec(ctx, `UPDATE nodes SET version=version+1,updated_at=$2 WHERE id=$1`, nodeID, now); err != nil {
		return nil, fmt.Errorf("prepare legacy pending node: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO node_endpoint_keys (node_id,endpoint_id,state,bound_at) VALUES ($1,$2,'pending',$3)`, nodeID, request.GetEndpointId(), now); err != nil {
		return nil, fmt.Errorf("bind pending endpoint: %w", err)
	}
	sealingKeys := slices.Clone(request.GetSealingKeys())
	slices.SortFunc(sealingKeys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	for _, key := range sealingKeys {
		if _, err := tx.Exec(ctx, `INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6)`, nodeID, key.GetPurpose(), key.GetVersion(), key.GetKeyId(), key.GetPublicKeySha256(), now); err != nil {
			return nil, fmt.Errorf("record password sealing key: %w", err)
		}
	}
	for _, capability := range normalizedCapabilities(request.GetCapabilities()) {
		if _, err := tx.Exec(ctx, `INSERT INTO node_capabilities (node_id,capability,approved) VALUES ($1,$2,false)`, nodeID, capability); err != nil {
			return nil, fmt.Errorf("record requested capability: %w", err)
		}
	}
	command, err := tx.Exec(ctx, `UPDATE enrollment_tokens SET consumed_at=$1,consumed_node_id=$2 WHERE id=$3 AND consumed_at IS NULL`, now, nodeID, tokenID)
	if err != nil {
		return nil, fmt.Errorf("consume enrollment token: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil, ErrInvalidToken
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: fmt.Sprintf("endpoint:%x", request.GetEndpointId()), Action: "node.enroll", ResourceType: "node", ResourceID: nodeID, RequestID: uuid.Must(uuid.NewV7()).String(), At: now}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit enrollment: %w", err)
	}
	return &agentv1.EnrollResponse{Result: agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL, NodeId: nodeID[:], ControllerEndpointId: s.controllerEndpointID}, nil
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin node bootstrap enrollment: %w", err)
	}
	defer rollback(tx)
	var tokenID, workspaceID uuid.UUID
	var environment string
	var expectedName *string
	var expiresAt time.Time
	var boundEndpoint []byte
	var consumedNodeID *uuid.UUID
	var consumedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT id,workspace_id,expected_environment,expected_node_name,expires_at,
		bound_endpoint_id,consumed_node_id,consumed_at FROM node_bootstrap_tokens WHERE token_hash=$1 FOR UPDATE`, digest[:]).
		Scan(&tokenID, &workspaceID, &environment, &expectedName, &expiresAt, &boundEndpoint, &consumedNodeID, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("lock node bootstrap token: %w", err)
	}
	if environment != request.GetEnvironment() {
		return nil, ErrInvalidToken
	}
	if consumedAt != nil {
		if consumedNodeID == nil || len(boundEndpoint) != 32 || subtle.ConstantTimeCompare(boundEndpoint, request.GetEndpointId()) != 1 {
			return nil, ErrEndpointMismatch
		}
		var status, endpointState string
		var persistedEndpoint []byte
		err := tx.QueryRow(ctx, `SELECT n.status,k.state,k.endpoint_id FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1`, *consumedNodeID).
			Scan(&status, &endpointState, &persistedEndpoint)
		if err != nil || status != "pending" || endpointState != "pending" || subtle.ConstantTimeCompare(persistedEndpoint, request.GetEndpointId()) != 1 {
			return nil, ErrInvalidToken
		}
		return &agentv1.EnrollResponse{Result: agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL, NodeId: (*consumedNodeID)[:], ControllerEndpointId: s.controllerEndpointID}, nil
	}
	if !expiresAt.After(s.now()) || len(boundEndpoint) != 0 || consumedNodeID != nil {
		return nil, ErrInvalidToken
	}
	if err := audit.LockChain(ctx, tx, workspaceID); err != nil {
		return nil, err
	}
	var endpointExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_endpoint_keys WHERE endpoint_id=$1)`, request.GetEndpointId()).Scan(&endpointExists); err != nil {
		return nil, fmt.Errorf("check bootstrap endpoint binding: %w", err)
	}
	if endpointExists {
		return nil, ErrEndpointMismatch
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE workspace_id=$1 AND status='pending'`, workspaceID).Scan(&pending); err != nil {
		return nil, fmt.Errorf("count pending nodes: %w", err)
	}
	if pending >= MaxPendingNodes {
		return nil, ErrPendingLimit
	}
	now := s.now().UTC()
	nodeID := uuid.Must(uuid.NewV7())
	name := "node-" + fmt.Sprintf("%x", request.GetEndpointId()[:6])
	if expectedName != nil {
		name = *expectedName
	}
	if _, err := tx.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'pending',$4,$4)`, nodeID, workspaceID, name, now); err != nil {
		return nil, fmt.Errorf("insert bootstrap pending node: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'pending',$3)`, nodeID, request.GetEndpointId(), now); err != nil {
		return nil, fmt.Errorf("bind bootstrap endpoint: %w", err)
	}
	sealingKeys := slices.Clone(request.GetSealingKeys())
	slices.SortFunc(sealingKeys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	for _, key := range sealingKeys {
		if _, err := tx.Exec(ctx, `INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6)`, nodeID, key.GetPurpose(), key.GetVersion(), key.GetKeyId(), key.GetPublicKeySha256(), now); err != nil {
			return nil, fmt.Errorf("record bootstrap password sealing key: %w", err)
		}
	}
	for _, capability := range normalizedCapabilities(request.GetCapabilities()) {
		if _, err := tx.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,false)`, nodeID, capability); err != nil {
			return nil, fmt.Errorf("record bootstrap requested capability: %w", err)
		}
	}
	command, err := tx.Exec(ctx, `UPDATE node_bootstrap_tokens SET bound_endpoint_id=$1,consumed_node_id=$2,consumed_at=$3 WHERE id=$4 AND consumed_at IS NULL`, request.GetEndpointId(), nodeID, now, tokenID)
	if err != nil {
		return nil, fmt.Errorf("consume node bootstrap token: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil, ErrInvalidToken
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: fmt.Sprintf("endpoint:%x", request.GetEndpointId()), Action: "node.enroll", ResourceType: "node", ResourceID: nodeID, RequestID: uuid.Must(uuid.NewV7()).String(), At: now}); err != nil {
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return NodeTrust{}, fmt.Errorf("begin approval: %w", err)
	}
	defer rollback(tx)
	var workspaceID uuid.UUID
	var endpointID []byte
	var currentStatus string
	var revision uint64
	var nodeVersion int64
	err = tx.QueryRow(ctx, `SELECT n.workspace_id,k.endpoint_id,n.status,n.authorization_revision,n.version FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 AND n.status IN ('pending','active','offline') FOR UPDATE OF n,k`, approval.NodeID).Scan(&workspaceID, &endpointID, &currentStatus, &revision, &nodeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeTrust{}, ErrInvalidTransition
	}
	if err != nil {
		return NodeTrust{}, fmt.Errorf("lock pending node: %w", err)
	}
	if currentStatus == "active" || currentStatus == "offline" {
		// Activation increments the node version once. Reconstruct the original
		// approved content so an idempotent retry cannot substitute policy,
		// labels, or capabilities after approval.
		if nodeVersion < 2 {
			return NodeTrust{}, ErrInvalidTransition
		}
		requestHash, _, bindingErr := nodeApprovalBinding(ctx, tx, approval.NodeID, endpointID, nodeVersion-1, approval.Labels, approval.Policy, capabilities)
		if bindingErr != nil {
			return NodeTrust{}, bindingErr
		}
		if err := approvalstore.ValidateConsumedBound(ctx, tx, approval.ApprovalID, workspaceID, approval.IdentityID, "node.approve", "node", approval.NodeID, requestHash); err != nil {
			return NodeTrust{}, err
		}
		return NodeTrust{NodeID: approval.NodeID, EndpointID: endpointID, Revision: revision}, nil
	}
	requestHash, _, err := nodeApprovalBinding(ctx, tx, approval.NodeID, endpointID, nodeVersion, approval.Labels, approval.Policy, capabilities)
	if err != nil {
		return NodeTrust{}, err
	}
	if err := approvalstore.ConsumeBound(ctx, tx, approval.ApprovalID, workspaceID, approval.IdentityID, "node.approve", "node", approval.NodeID, requestHash); err != nil {
		return NodeTrust{}, err
	}
	labels := mapToJSON(approval.Labels)
	now := s.now().UTC()
	if err := tx.QueryRow(ctx, `UPDATE nodes SET status='active',labels=$2::jsonb,policy=$3,version=version+1,authorization_revision=authorization_revision+1,updated_at=$4 WHERE id=$1 RETURNING authorization_revision`, approval.NodeID, labels, approval.Policy, now).Scan(&revision); err != nil {
		return NodeTrust{}, fmt.Errorf("activate node: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE node_endpoint_keys SET state='active' WHERE node_id=$1`, approval.NodeID); err != nil {
		return NodeTrust{}, fmt.Errorf("activate endpoint: %w", err)
	}
	if err := enqueueTrustConvergence(ctx, tx, approval.NodeID, endpointID, "active", revision, approval.Reason, now); err != nil {
		return NodeTrust{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_capabilities SET approved=false WHERE node_id=$1`, approval.NodeID); err != nil {
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
		if _, err := tx.Exec(ctx, `INSERT INTO node_capabilities (node_id,capability,approved) VALUES ($1,$2,true) ON CONFLICT (node_id,capability) DO UPDATE SET approved=true`, approval.NodeID, capability); err != nil {
			return NodeTrust{}, fmt.Errorf("approve capability: %w", err)
		}
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: approval.ActorID, SessionID: &approval.SessionID, ApprovalID: &approval.ApprovalID, NodeID: &approval.NodeID, Action: "node.approve", ResourceType: "node", ResourceID: approval.NodeID, RequestID: approval.RequestID, Reason: approval.Reason, At: now}); err != nil {
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
	var workspaceID uuid.UUID
	var endpointID []byte
	var version int64
	if err := s.pool.QueryRow(ctx, `SELECT n.workspace_id,k.endpoint_id,n.version FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 AND n.status='pending' AND k.state='pending'`, nodeID).Scan(&workspaceID, &endpointID, &version); err != nil {
		return uuid.Nil, nil, nil, err
	}
	hash, summary, err := nodeApprovalBinding(ctx, s.pool, nodeID, endpointID, version, labels, policy, capabilities)
	return workspaceID, hash, summary, err
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func nodeApprovalBinding(ctx context.Context, q queryRower, nodeID uuid.UUID, endpointID []byte, version int64, labels map[string]string, policy string, capabilities []string) ([]byte, json.RawMessage, error) {
	for _, capability := range capabilities {
		var supported bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=$1 AND capability=$2)`, nodeID, capability).Scan(&supported); err != nil {
			return nil, nil, err
		}
		if !supported {
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return NodeTrust{}, fmt.Errorf("begin revocation: %w", err)
	}
	defer rollback(tx)
	var workspaceID uuid.UUID
	var endpointID []byte
	var currentStatus string
	var revision uint64
	err = tx.QueryRow(ctx, `SELECT n.workspace_id,k.endpoint_id,n.status,n.authorization_revision FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 FOR UPDATE OF n,k`, revocation.NodeID).Scan(&workspaceID, &endpointID, &currentStatus, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeTrust{}, ErrInvalidTransition
	}
	if err != nil {
		return NodeTrust{}, fmt.Errorf("lock node for revocation: %w", err)
	}
	revokeHash, _ := approvalstore.GenericBinding("node.revoke", "node", revocation.NodeID)
	if currentStatus == "revoked" {
		if err := approvalstore.ValidateConsumedBound(ctx, tx, revocation.ApprovalID, workspaceID, revocation.IdentityID, "node.revoke", "node", revocation.NodeID, revokeHash); err != nil {
			return NodeTrust{}, err
		}
		return NodeTrust{NodeID: revocation.NodeID, EndpointID: endpointID, Revision: revision}, nil
	}
	if err := approvalstore.ConsumeBound(ctx, tx, revocation.ApprovalID, workspaceID, revocation.IdentityID, "node.revoke", "node", revocation.NodeID, revokeHash); err != nil {
		return NodeTrust{}, err
	}
	now := s.now().UTC()
	if err := tx.QueryRow(ctx, `UPDATE nodes SET status='revoked',version=version+1,authorization_revision=authorization_revision+1,updated_at=$2 WHERE id=$1 RETURNING authorization_revision`, revocation.NodeID, now).Scan(&revision); err != nil {
		return NodeTrust{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=$2 WHERE node_id=$1`, revocation.NodeID, now); err != nil {
		return NodeTrust{}, err
	}
	if err := enqueueTrustConvergence(ctx, tx, revocation.NodeID, endpointID, "revoked", revision, revocation.Reason, now); err != nil {
		return NodeTrust{}, err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: revocation.ActorID, SessionID: &revocation.SessionID, ApprovalID: &revocation.ApprovalID, NodeID: &revocation.NodeID, Action: "node.revoke", ResourceType: "node", ResourceID: revocation.NodeID, RequestID: revocation.RequestID, Reason: revocation.Reason, At: now}); err != nil {
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
	if request.GetAlpn() == "ocserv-platform/enroll/1" {
		var permitted bool
		err := s.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM node_endpoint_keys WHERE endpoint_id=$1 AND state='revoked')`, request.GetEndpointId()).Scan(&permitted)
		return permitted, err
	}
	if request.GetAlpn() != "ocserv-platform/agent/1" {
		return false, nil
	}
	var permitted bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE k.endpoint_id=$1 AND n.status IN ('active','offline') AND k.state='active')`, request.GetEndpointId()).Scan(&permitted)
	return permitted, err
}

func (s *Service) ListNodeTrust(ctx context.Context) ([]*transportv1.NodeTrustBinding, error) {
	rows, err := s.pool.Query(ctx, `SELECT n.id,k.endpoint_id,
		CASE WHEN n.status='revoked' OR k.state='revoked' THEN 'revoked' ELSE 'active' END,
		n.authorization_revision
		FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id
		WHERE (n.status IN ('active','offline') AND k.state='active')
		   OR k.state='revoked'
		ORDER BY n.id`)
	if err != nil {
		return nil, fmt.Errorf("list node trust snapshot: %w", err)
	}
	defer rows.Close()
	bindings := make([]*transportv1.NodeTrustBinding, 0)
	for rows.Next() {
		var nodeID uuid.UUID
		var endpointID []byte
		var state string
		var revision uint64
		if err := rows.Scan(&nodeID, &endpointID, &state, &revision); err != nil {
			return nil, fmt.Errorf("scan node trust snapshot: %w", err)
		}
		trustState := transportv1.NodeTrustState_NODE_TRUST_STATE_ACTIVE
		if state == "revoked" {
			trustState = transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED
		}
		bindings = append(bindings, &transportv1.NodeTrustBinding{NodeId: nodeID[:], EndpointId: endpointID, State: trustState, Revision: revision})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node trust snapshot: %w", err)
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
	var status string
	var endpointState string
	var nodeID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT n.id,n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE k.endpoint_id=$1`, request.GetRemoteEndpointId()).Scan(&nodeID, &status, &endpointState)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authorize endpoint: %w", err)
	}
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, fmt.Errorf("begin session authorization: %w", err)
	}
	defer rollback(tx)
	var authorizationRevision uint64
	err = tx.QueryRow(ctx, `SELECT n.id,n.status,k.state,n.authorization_revision FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE k.endpoint_id=$1 FOR SHARE OF n,k`, request.GetRemoteEndpointId()).Scan(&nodeID, &status, &endpointState, &authorizationRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock session authority: %w", err)
	}
	if (status != "active" && status != "offline") || endpointState != "active" || authorizationRevision == 0 || !slices.Equal(nodeID[:], handshake.GetNodeId()) {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED
		return response, nil
	}
	matchingSealingKeys, err := sealingKeysMatch(ctx, tx, nodeID, handshake.GetSealingKeys())
	if err != nil {
		return nil, fmt.Errorf("verify session sealing keys: %w", err)
	}
	legacyReadOnlySealingFallback := len(handshake.GetSealingKeys()) == 0
	if !matchingSealingKeys && !legacyReadOnlySealingFallback {
		response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_CAPABILITY_REJECTED
		return response, nil
	}
	rows, err := tx.Query(ctx, `SELECT capability FROM node_capabilities WHERE node_id=$1 AND approved=true ORDER BY capability`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	approved := map[string]struct{}{}
	for rows.Next() {
		var capability string
		if err := rows.Scan(&capability); err != nil {
			return nil, err
		}
		approved[capability] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
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

func sealingKeysMatch(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, advertised []*agentv1.SealingKeyDescriptorV1) (bool, error) {
	keys := slices.Clone(advertised)
	slices.SortFunc(keys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	if err := validateSealingKeys(keys); err != nil {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT purpose,version,key_id,public_key_sha256 FROM node_sealing_keys WHERE node_id=$1 ORDER BY purpose`, nodeID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		if index >= len(keys) {
			return false, nil
		}
		var purpose, version int32
		var keyID string
		var digest []byte
		if err := rows.Scan(&purpose, &version, &keyID, &digest); err != nil {
			return false, err
		}
		key := keys[index]
		if purpose != int32(key.GetPurpose()) || version != int32(key.GetVersion()) || keyID != key.GetKeyId() || subtle.ConstantTimeCompare(digest, key.GetPublicKeySha256()) != 1 {
			return false, nil
		}
		index++
	}
	return index == len(keys), rows.Err()
}

func supportedCapabilitiesMatch(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, advertised []string) (bool, error) {
	want := normalizedCapabilities(advertised)
	rows, err := tx.Query(ctx, `SELECT capability FROM node_capabilities WHERE node_id=$1 ORDER BY capability`, nodeID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	have := make([]string, 0, len(want))
	for rows.Next() {
		var capability string
		if err := rows.Scan(&capability); err != nil {
			return false, err
		}
		have = append(have, capability)
	}
	return slices.Equal(have, want), rows.Err()
}

func enqueueTrustConvergence(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, endpointID []byte, state string, revision uint64, reason string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO node_trust_convergence
		(node_id,endpoint_id,desired_state,revision,reason,close_required,available_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$3::text='revoked',$6,$6,$6)
		ON CONFLICT(node_id) DO UPDATE SET endpoint_id=EXCLUDED.endpoint_id,desired_state=EXCLUDED.desired_state,
		revision=EXCLUDED.revision,reason=EXCLUDED.reason,update_applied=false,close_required=EXCLUDED.close_required,
		close_applied=false,available_at=EXCLUDED.available_at,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=EXCLUDED.updated_at
		WHERE node_trust_convergence.revision < EXCLUDED.revision`, nodeID, endpointID, state, revision, reason, now)
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
func validateLockedToken(queryErr error, consumed bool, expiresAt, now time.Time) error {
	if errors.Is(queryErr, pgx.ErrNoRows) {
		return ErrInvalidToken
	}
	if queryErr != nil {
		return fmt.Errorf("lock enrollment token: %w", queryErr)
	}
	if consumed || !now.Before(expiresAt) {
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
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
