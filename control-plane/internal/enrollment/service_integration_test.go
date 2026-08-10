package enrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	approvalstore "github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCreateTokenUnknownWorkspaceIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	service := newTestService(t, pool, "", "test")
	_, err = service.CreateToken(ctx, TokenSpec{WorkspaceID: uuid.Must(uuid.NewV7()), Environment: "test", ActorID: "integration", Reason: "unknown workspace", RequestID: uuid.Must(uuid.NewV7()).String()})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown workspace error=%v", err)
	}
}

func TestConcurrentAuditChainIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Audit concurrency',$2,now(),now())`, workspaceID, "audit-"+workspaceID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupWorkspace(ctx, pool, workspaceID)

	service := newTestService(t, pool, "", "test")
	endpoint := bytes.Repeat([]byte{0x2a}, 32)
	enrollmentToken, err := service.CreateToken(ctx, TokenSpec{WorkspaceID: workspaceID, Environment: "test", ExpectedEndpointID: endpoint, ActorID: "integration", Reason: "concurrent enrollment", RequestID: "audit-enrollment-token"})
	if err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 8)
	var wait sync.WaitGroup
	for index := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if index == 0 {
				_, enrollErr := service.Enroll(ctx, enrollmentRequest(enrollmentToken.Value, endpoint))
				errors <- enrollErr
				return
			}
			_, createErr := service.CreateToken(ctx, TokenSpec{WorkspaceID: workspaceID, Environment: "test", ActorID: "integration", Reason: "concurrent audit", RequestID: fmt.Sprintf("audit-%d", index)})
			errors <- createErr
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	rows, err := pool.Query(ctx, `SELECT previous_event_hash,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at,id`, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var previous []byte
	count := 0
	for rows.Next() {
		var linked, current []byte
		if err := rows.Scan(&linked, &current); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(linked, previous) {
			t.Fatalf("audit row %d does not link to its serialized predecessor", count)
		}
		previous = current
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 9 {
		t.Fatalf("audit row count=%d", count)
	}
}

func TestEnrollmentTrustLifecycleIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Enrollment integration',$2,now(),now())`, workspaceID, "enrollment-"+workspaceID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupWorkspace(ctx, pool, workspaceID)

	service := newTestService(t, pool, string(make([]byte, 64)), "test")
	endpoint := make([]byte, 32)
	endpoint[0] = 7
	token := createToken(t, service, workspaceID, endpoint)
	request := enrollmentRequest(token.Value, endpoint)

	var successes atomic.Int32
	nodeIDs := make(chan string, 12)
	var wait sync.WaitGroup
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, enrollErr := service.Enroll(ctx, request)
			if enrollErr == nil && response.GetResult() == agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL {
				successes.Add(1)
				nodeIDs <- string(response.GetNodeId())
			}
		}()
	}
	wait.Wait()
	close(nodeIDs)
	if successes.Load() < 1 {
		t.Fatalf("concurrent token successes = %d", successes.Load())
	}
	var concurrentNodeID string
	for candidate := range nodeIDs {
		if concurrentNodeID == "" {
			concurrentNodeID = candidate
		}
		if candidate != concurrentNodeID {
			t.Fatal("concurrent enrollment retries returned different nodes")
		}
	}
	retry, err := service.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("pending retry response = %v, %v", retry, err)
	}
	permitted, err := service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/enroll/1"})
	if err != nil || !permitted {
		t.Fatalf("pending enrollment endpoint permitted=%v err=%v", permitted, err)
	}
	permitted, err = service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"})
	if err != nil || permitted {
		t.Fatalf("pending endpoint permitted=%v err=%v", permitted, err)
	}

	var nodeID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT consumed_node_id FROM enrollment_tokens WHERE id=$1`, token.ID).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	if string(retry.GetNodeId()) != string(nodeID[:]) {
		t.Fatal("pending enrollment retry returned a different node")
	}
	if concurrentNodeID != string(nodeID[:]) {
		t.Fatal("concurrent enrollment retry returned a different node")
	}
	if _, err := pool.Exec(ctx, `UPDATE enrollment_tokens SET created_at=now()-interval '2 seconds', expires_at=now()-interval '1 second' WHERE id=$1`, token.ID); err != nil {
		t.Fatal(err)
	}
	expiredRetry, err := service.Enroll(ctx, request)
	if err != nil || string(expiredRetry.GetNodeId()) != string(nodeID[:]) {
		t.Fatalf("expired consumed-token retry = %v, %v", expiredRetry, err)
	}
	approvalID, identityID, sessionID := approvedMetadata(t, pool, workspaceID, nodeID, "node.approve")
	trust, err := service.Approve(ctx, Approval{NodeID: nodeID, Labels: map[string]string{"region": "test"}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: identityID.String(), ApprovalID: approvalID, IdentityID: identityID, SessionID: sessionID, Reason: "approve fixture", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	approvedRetry, err := service.Enroll(ctx, request)
	if err != nil || approvedRetry.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || string(approvedRetry.GetNodeId()) != string(nodeID[:]) {
		t.Fatalf("approved enrollment recovery = %v, %v", approvedRetry, err)
	}
	permitted, err = service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/enroll/1"})
	if err != nil || !permitted {
		t.Fatalf("active enrollment recovery permitted=%v err=%v", permitted, err)
	}
	handshake := &agentv1.SessionHandshake{ProtocolMajor: 1, ProtocolMinor: 0, AgentVersion: "test", NodeId: nodeID[:], EndpointId: endpoint, Capabilities: []string{"ocserv.status.read"}, OsRelease: "test", BootId: "boot", AgentInstanceId: uuidBytes(), MaxMessageSize: 1024, Time: timestamppb.Now(), Nonce: make([]byte, 16)}
	response, err := service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED {
		t.Fatalf("active authorization = %v, %v", response, err)
	}
	if !slices.Equal(response.GetNegotiatedCapabilities(), []string{"ocserv.status.read"}) || response.GetSessionGrant() != nil {
		t.Fatalf("legacy authorization negotiated=%v grant=%v", response.GetNegotiatedCapabilities(), response.GetSessionGrant())
	}
	handshake.ProtocolMajor = 2
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_INCOMPATIBLE_PROTOCOL {
		t.Fatalf("major protocol mismatch = %v, %v", response, err)
	}
	if response.GetControllerVersion() != "test" {
		t.Fatalf("controller version = %q", response.GetControllerVersion())
	}
	handshake.ProtocolMajor = 1
	handshake.ProtocolMinor = ProtocolMinor + 1
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_UPGRADE_REQUIRED {
		t.Fatalf("minor protocol mismatch = %v, %v", response, err)
	}
	handshake.ProtocolMinor = ProtocolMinor
	handshake.Capabilities = []string{"ocserv.status.read", "ocserv.users.write", "unsupported.agent.feature"}
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED {
		t.Fatalf("subset capability authorization = %v, %v", response, err)
	}
	if !slices.Equal(response.GetNegotiatedCapabilities(), []string{"ocserv.status.read"}) {
		t.Fatalf("negotiated capabilities = %v", response.GetNegotiatedCapabilities())
	}
	grant := response.GetSessionGrant()
	if grant == nil || grant.GetAuthorizationRevision() != trust.Revision || !slices.Equal(grant.GetNegotiatedCapabilities(), response.GetNegotiatedCapabilities()) {
		t.Fatalf("signed session grant = %v", grant)
	}
	var signedNode [16]byte
	var signedEndpoint [32]byte
	copy(signedNode[:], grant.GetNodeId())
	copy(signedEndpoint[:], grant.GetEndpointId())
	canonical, err := commandauth.CanonicalSessionGrantV1(commandauth.SessionGrantClaimsV1{
		Version: uint32(grant.GetVersion()), KeyID: grant.GetKeyId(),
		ProtocolMajor: grant.GetProtocolMajor(), ProtocolMinor: grant.GetProtocolMinor(),
		NodeID: signedNode, EndpointID: signedEndpoint,
		AuthorizationRevision:  grant.GetAuthorizationRevision(),
		NegotiatedCapabilities: grant.GetNegotiatedCapabilities(),
		IssuedAtSeconds:        grant.GetIssuedAt().GetSeconds(), IssuedAtNanos: uint32(grant.GetIssuedAt().GetNanos()),
		ExpiresAtSeconds: grant.GetExpiresAt().GetSeconds(), ExpiresAtNanos: uint32(grant.GetExpiresAt().GetNanos()),
	})
	if err != nil || !ed25519.Verify(service.signer.PublicKey(), canonical, grant.GetSignature()) {
		t.Fatalf("Controller session grant signature invalid: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_capabilities SET approved=false WHERE node_id=$1 AND capability='ocserv.status.read'`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE nodes SET authorization_revision=authorization_revision+1 WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	handshake.Capabilities = []string{"ocserv.status.read"}
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || len(response.GetNegotiatedCapabilities()) != 0 || response.GetSessionGrant().GetAuthorizationRevision() != trust.Revision+1 {
		t.Fatalf("revoked capability session authority = %v, %v", response, err)
	}
	handshake.ProtocolMinor = 0
	handshake.Capabilities = []string{"ocserv.status.read"}
	if _, err := pool.Exec(ctx, `UPDATE nodes SET status='offline' WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	permitted, err = service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"})
	if err != nil || !permitted {
		t.Fatalf("offline endpoint permitted=%v err=%v", permitted, err)
	}
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED {
		t.Fatalf("offline authorization = %v, %v", response, err)
	}
	if retryTrust, err := service.Approve(ctx, Approval{NodeID: nodeID, Labels: map[string]string{"region": "test"}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: identityID.String(), ApprovalID: approvalID, IdentityID: identityID, SessionID: sessionID, Reason: "retry approval", RequestID: uuid.Must(uuid.NewV7()).String()}); err != nil || retryTrust.NodeID != nodeID {
		t.Fatalf("offline approval retry = %v, %v", retryTrust, err)
	}
	if _, err := service.Approve(ctx, Approval{NodeID: nodeID, Labels: map[string]string{"region": "test"}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: identityID.String(), ApprovalID: uuid.Must(uuid.NewV7()), IdentityID: identityID, SessionID: sessionID, Reason: "invalid retry approval", RequestID: uuid.Must(uuid.NewV7()).String()}); !errors.Is(err, approvalstore.ErrNotReady) {
		t.Fatalf("offline approval with unrelated credential error = %v", err)
	}
	handshake.Time = timestamppb.New(time.Now().Add(-MaxClockSkew - time.Second))
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_CLOCK_SKEW {
		t.Fatalf("clock skew authorization = %v, %v", response, err)
	}
	revokeApprovalID, revokeIdentityID, revokeSessionID := approvedMetadata(t, pool, workspaceID, nodeID, "node.revoke")
	if _, err := service.Revoke(ctx, Revocation{NodeID: trust.NodeID, ActorID: revokeIdentityID.String(), ApprovalID: revokeApprovalID, IdentityID: revokeIdentityID, SessionID: revokeSessionID, Reason: "revoke fixture", RequestID: uuid.Must(uuid.NewV7()).String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Revoke(ctx, Revocation{NodeID: trust.NodeID, ActorID: revokeIdentityID.String(), ApprovalID: uuid.Must(uuid.NewV7()), IdentityID: revokeIdentityID, SessionID: revokeSessionID, Reason: "invalid retry revocation", RequestID: uuid.Must(uuid.NewV7()).String()}); !errors.Is(err, approvalstore.ErrNotReady) {
		t.Fatalf("revoked node with unrelated credential error = %v", err)
	}
	if _, err := service.Revoke(ctx, Revocation{NodeID: trust.NodeID, ActorID: revokeIdentityID.String(), ApprovalID: revokeApprovalID, IdentityID: revokeIdentityID, SessionID: revokeSessionID, Reason: "retry revocation", RequestID: uuid.Must(uuid.NewV7()).String()}); err != nil {
		t.Fatalf("revoked node retry error = %v", err)
	}
	permitted, err = service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"})
	if err != nil || permitted {
		t.Fatalf("revoked endpoint permitted=%v err=%v", permitted, err)
	}
	permitted, err = service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/enroll/1"})
	if err != nil || permitted {
		t.Fatalf("revoked enrollment endpoint permitted=%v err=%v", permitted, err)
	}
	if _, err := service.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token replay error = %v", err)
	}
}

func TestExpiredAndSubstitutedEnrollmentTokensIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Token rejection',$2,now(),now())`, workspaceID, "reject-"+workspaceID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupWorkspace(ctx, pool, workspaceID)
	service := newTestService(t, pool, "", "test")
	endpoint := make([]byte, 32)
	endpoint[0] = 9
	token := createToken(t, service, workspaceID, endpoint)
	substitute := make([]byte, 32)
	substitute[0] = 10
	if _, err := service.Enroll(ctx, enrollmentRequest(token.Value, substitute)); !errors.Is(err, ErrEndpointMismatch) {
		t.Fatalf("substitution error=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE enrollment_tokens SET created_at=now()-interval '2 seconds', expires_at=now()-interval '1 second' WHERE id=$1`, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enroll(ctx, enrollmentRequest(token.Value, endpoint)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expiry error=%v", err)
	}
}

func TestLegacyPendingNodeCanBeReenrolledIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	legacyNodeID := uuid.Must(uuid.NewV7())
	legacyName := "legacy-node"
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Legacy enrollment',$2,now(),now())`, workspaceID, "legacy-"+workspaceID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupWorkspace(ctx, pool, workspaceID)
	_, err = pool.Exec(ctx, `INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ($1,$2,$3,'pending',now(),now())`, legacyNodeID, workspaceID, legacyName)
	if err != nil {
		t.Fatal(err)
	}

	service := newTestService(t, pool, "", "test")
	endpoint := make([]byte, 32)
	endpoint[0] = 11
	token, err := service.CreateToken(ctx, TokenSpec{WorkspaceID: workspaceID, Environment: "test", ExpectedNodeName: legacyName, ExpectedEndpointID: endpoint, ActorID: "integration", Reason: "re-enroll legacy node", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Enroll(ctx, enrollmentRequest(token.Value, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	if string(response.GetNodeId()) != string(legacyNodeID[:]) {
		t.Fatal("legacy enrollment created a replacement node")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM node_endpoint_keys WHERE node_id=$1 AND endpoint_id=$2`, legacyNodeID, endpoint).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("legacy endpoint binding state=%q err=%v", state, err)
	}
}

func createToken(t *testing.T, service *Service, workspaceID uuid.UUID, endpoint []byte) Token {
	t.Helper()
	token, err := service.CreateToken(context.Background(), TokenSpec{WorkspaceID: workspaceID, Environment: "test", ExpectedEndpointID: endpoint, ActorID: "integration", Reason: "test enrollment", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func newTestService(t *testing.T, pool *pgxpool.Pool, controllerEndpointID, controllerVersion string) *Service {
	t.Helper()
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	return New(pool, controllerEndpointID, controllerVersion, signer)
}

func enrollmentRequest(token string, endpoint []byte) *agentv1.EnrollRequest {
	return &agentv1.EnrollRequest{Token: token, EndpointId: endpoint, AgentVersion: "test", OsRelease: "test", OcservVersion: "test", BootId: "boot", AgentInstanceId: uuidBytes(), Capabilities: []string{"ocserv.status.read"}, Environment: "test", Nonce: make([]byte, 16), Time: timestamppb.Now()}
}

func uuidBytes() []byte {
	id := uuid.Must(uuid.NewV7())
	return id[:]
}

func cleanupWorkspace(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID) {
	_, _ = pool.Exec(ctx, `DELETE FROM audit_events WHERE workspace_id=$1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM enrollment_tokens WHERE workspace_id=$1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM node_capabilities WHERE node_id IN (SELECT id FROM nodes WHERE workspace_id=$1)`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM node_endpoint_keys WHERE node_id IN (SELECT id FROM nodes WHERE workspace_id=$1)`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM nodes WHERE workspace_id=$1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspaceID)
}

func approvedMetadata(t *testing.T, pool *pgxpool.Pool, workspaceID, resourceID uuid.UUID, action string) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	requesterID, approverID, sessionID, approvalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, requesterID, "requester-"+requesterID.String(), approverID, "approver-"+approverID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, sessionID, requesterID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at) VALUES($1,$2,$3,$4,'node',$5,'integration','approved',$6,'independent',now()+interval '1 hour',now(),now())`, approvalID, workspaceID, requesterID, action, resourceID, approverID); err != nil {
		t.Fatal(err)
	}
	return approvalID, requesterID, sessionID
}
