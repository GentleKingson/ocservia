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
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultTokenTTL = 15 * time.Minute
	MaxPendingNodes = 100
	MaxClockSkew    = 5 * time.Minute
	ProtocolMajor   = 1
	ProtocolMinor   = 0
	MaxMessageSize  = 1 << 20
)

var (
	ErrInvalidToken      = errors.New("enrollment token is invalid or expired")
	ErrEndpointMismatch  = errors.New("endpoint does not match enrollment token")
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

type Token struct {
	ID        uuid.UUID
	Value     string
	ExpiresAt time.Time
}

type Approval struct {
	NodeID       uuid.UUID
	Labels       map[string]string
	Policy       string
	Capabilities []string
	ActorID      string
	Reason       string
	RequestID    string
}

type Revocation struct {
	NodeID    uuid.UUID
	ActorID   string
	Reason    string
	RequestID string
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
}

func New(pool *pgxpool.Pool, controllerEndpointID, controllerVersion string) *Service {
	return &Service{pool: pool, now: time.Now, random: rand.Reader, controllerEndpointID: controllerEndpointID, controllerVersion: controllerVersion}
}

func (s *Service) CreateToken(ctx context.Context, spec TokenSpec) (Token, error) {
	if spec.WorkspaceID == uuid.Nil || !validShort(spec.Environment, 64) || !validOptional(spec.ExpectedNodeName, 128) ||
		(len(spec.ExpectedEndpointID) != 0 && len(spec.ExpectedEndpointID) != 32) || !validActor(spec.ActorID, spec.RequestID, spec.Reason) {
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
	var expectedEndpoint any
	if len(spec.ExpectedEndpointID) != 0 {
		expectedEndpoint = spec.ExpectedEndpointID
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
		token.ID, spec.WorkspaceID, digest[:], spec.Environment, expectedName, expectedEndpoint, token.ExpiresAt, spec.ActorID, now)
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

func (s *Service) Enroll(ctx context.Context, request *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	if err := validateEnrollment(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
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
	var consumedNodeID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id, workspace_id, expected_environment, expected_node_name,
	        expected_endpoint_id, expires_at, consumed_at, consumed_node_id FROM enrollment_tokens WHERE token_hash=$1 FOR UPDATE`, digest[:]).
		Scan(&tokenID, &workspaceID, &environment, &expectedName, &expectedEndpoint, &expiresAt, &consumedAt, &consumedNodeID)
	if err := validateLockedToken(err, consumedAt != nil, expiresAt, s.now()); err != nil {
		return nil, err
	}
	if environment != request.GetEnvironment() {
		return nil, ErrInvalidToken
	}
	if len(expectedEndpoint) != 0 && subtle.ConstantTimeCompare(expectedEndpoint, request.GetEndpointId()) != 1 {
		return nil, ErrEndpointMismatch
	}
	if consumedAt != nil {
		if consumedNodeID == nil {
			return nil, ErrInvalidToken
		}
		var nodeStatus, endpointState string
		err := tx.QueryRow(ctx, `SELECT n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 AND k.endpoint_id=$2`, *consumedNodeID, request.GetEndpointId()).Scan(&nodeStatus, &endpointState)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrInvalidToken
			}
			return nil, fmt.Errorf("check enrollment retry: %w", err)
		}
		result := agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL
		if (nodeStatus == "active" || nodeStatus == "offline") && endpointState == "active" {
			result = agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED
		} else if nodeStatus != "pending" || endpointState != "pending" {
			return nil, ErrInvalidToken
		}
		return &agentv1.EnrollResponse{Result: result, NodeId: (*consumedNodeID)[:], ControllerEndpointId: s.controllerEndpointID}, nil
	}
	if err := audit.LockChain(ctx, tx, workspaceID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
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

func (s *Service) Approve(ctx context.Context, approval Approval) (NodeTrust, error) {
	if approval.NodeID == uuid.Nil || !validActor(approval.ActorID, approval.RequestID, approval.Reason) || !validPolicy(approval.Policy) || len(approval.Labels) > 32 {
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
	err = tx.QueryRow(ctx, `SELECT n.workspace_id,k.endpoint_id,n.status,n.version FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 AND n.status IN ('pending','active','offline') FOR UPDATE OF n,k`, approval.NodeID).Scan(&workspaceID, &endpointID, &currentStatus, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeTrust{}, ErrInvalidTransition
	}
	if err != nil {
		return NodeTrust{}, fmt.Errorf("lock pending node: %w", err)
	}
	if currentStatus == "active" || currentStatus == "offline" {
		return NodeTrust{NodeID: approval.NodeID, EndpointID: endpointID, Revision: revision}, nil
	}
	labels := mapToJSON(approval.Labels)
	now := s.now().UTC()
	if err := tx.QueryRow(ctx, `UPDATE nodes SET status='active',labels=$2::jsonb,policy=$3,version=version+1,updated_at=$4 WHERE id=$1 RETURNING version`, approval.NodeID, labels, approval.Policy, now).Scan(&revision); err != nil {
		return NodeTrust{}, fmt.Errorf("activate node: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE node_endpoint_keys SET state='active' WHERE node_id=$1`, approval.NodeID); err != nil {
		return NodeTrust{}, fmt.Errorf("activate endpoint: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE node_capabilities SET approved=false WHERE node_id=$1`, approval.NodeID); err != nil {
		return NodeTrust{}, err
	}
	for _, capability := range capabilities {
		if _, err := tx.Exec(ctx, `INSERT INTO node_capabilities (node_id,capability,approved) VALUES ($1,$2,true) ON CONFLICT (node_id,capability) DO UPDATE SET approved=true`, approval.NodeID, capability); err != nil {
			return NodeTrust{}, fmt.Errorf("approve capability: %w", err)
		}
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: approval.ActorID, Action: "node.approve", ResourceType: "node", ResourceID: approval.NodeID, RequestID: approval.RequestID, Reason: approval.Reason, At: now}); err != nil {
		return NodeTrust{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NodeTrust{}, fmt.Errorf("commit approval: %w", err)
	}
	return NodeTrust{NodeID: approval.NodeID, EndpointID: endpointID, Revision: revision}, nil
}

func (s *Service) Revoke(ctx context.Context, revocation Revocation) (NodeTrust, error) {
	if revocation.NodeID == uuid.Nil || !validActor(revocation.ActorID, revocation.RequestID, revocation.Reason) {
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
	err = tx.QueryRow(ctx, `SELECT n.workspace_id,k.endpoint_id,n.status,n.version FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 FOR UPDATE OF n,k`, revocation.NodeID).Scan(&workspaceID, &endpointID, &currentStatus, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeTrust{}, ErrInvalidTransition
	}
	if err != nil {
		return NodeTrust{}, fmt.Errorf("lock node for revocation: %w", err)
	}
	if currentStatus == "revoked" {
		return NodeTrust{NodeID: revocation.NodeID, EndpointID: endpointID, Revision: revision}, nil
	}
	now := s.now().UTC()
	if err := tx.QueryRow(ctx, `UPDATE nodes SET status='revoked',version=version+1,updated_at=$2 WHERE id=$1 RETURNING version`, revocation.NodeID, now).Scan(&revision); err != nil {
		return NodeTrust{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=$2 WHERE node_id=$1`, revocation.NodeID, now); err != nil {
		return NodeTrust{}, err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: revocation.ActorID, Action: "node.revoke", ResourceType: "node", ResourceID: revocation.NodeID, RequestID: revocation.RequestID, Reason: revocation.Reason, At: now}); err != nil {
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
	rows, err := s.pool.Query(ctx, `SELECT capability FROM node_capabilities WHERE node_id=$1 AND approved=true`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	approved := map[string]bool{}
	for rows.Next() {
		var capability string
		if err := rows.Scan(&capability); err != nil {
			return nil, err
		}
		approved[capability] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, capability := range handshake.GetCapabilities() {
		if !approved[capability] {
			response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_CAPABILITY_REJECTED
			return response, nil
		}
	}
	response.Result = agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED
	response.MaxMessageSize = min(handshake.GetMaxMessageSize(), MaxMessageSize)
	return response, nil
}

func validateEnrollment(request *agentv1.EnrollRequest) error {
	if request == nil || len(request.GetEndpointId()) != 32 || !validShort(request.GetAgentVersion(), 128) || !validShort(request.GetOsRelease(), 256) ||
		!validShort(request.GetBootId(), 256) || len(request.GetAgentInstanceId()) != 16 || len(request.GetNonce()) < 16 || len(request.GetNonce()) > 64 ||
		request.GetTime() == nil || request.GetTime().CheckValid() != nil || !validShort(request.GetEnvironment(), 64) || len(request.GetCapabilities()) > 128 {
		return errors.New("invalid enrollment request")
	}
	if !validCapabilities(request.GetCapabilities()) {
		return errors.New("invalid enrollment capabilities")
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
	if !consumed && !now.Before(expiresAt) {
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
