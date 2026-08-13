package enrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	_, err = service.CreateToken(ctx, TokenSpec{WorkspaceID: uuid.Must(uuid.NewV7()), Environment: "test", ExpectedEndpointID: endpointFixture(1), ActorID: "integration", Reason: "unknown workspace", RequestID: uuid.Must(uuid.NewV7()).String()})
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
	endpoint := endpointFixture(2)
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
			_, createErr := service.CreateToken(ctx, TokenSpec{WorkspaceID: workspaceID, Environment: "test", ExpectedEndpointID: endpointFixture(byte(index + 2)), ActorID: "integration", Reason: "concurrent audit", RequestID: fmt.Sprintf("audit-%d", index)})
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
	endpoint := endpointFixture(7)
	token := createToken(t, service, workspaceID, endpoint)
	request := enrollmentRequest(token.Value, endpoint)
	if err := service.ValidateEnrollment(ctx, request); err != nil {
		t.Fatalf("valid first application message was rejected: %v", err)
	}

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
	if successes.Load() != 1 {
		t.Fatalf("concurrent one-time token successes = %d, want 1", successes.Load())
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
	if _, err := service.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("pending consumed-token replay error = %v", err)
	}
	if err := service.ValidateEnrollment(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("consumed token passed pre-completion validation: %v", err)
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
	if concurrentNodeID != string(nodeID[:]) {
		t.Fatal("successful concurrent enrollment returned a different node")
	}
	if _, err := pool.Exec(ctx, `UPDATE enrollment_tokens SET created_at=now()-interval '2 seconds', expires_at=now()-interval '1 second' WHERE id=$1`, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired consumed-token replay error = %v", err)
	}
	if _, _, _, err := service.ApprovalBinding(ctx, nodeID, map[string]string{"region": "test"}, "readonly", []string{"ocserv.status.read", "ocserv.user.manage"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unsupported approved capability err=%v", err)
	}
	_, approvalHash, approvalSummary, bindingErr := service.ApprovalBinding(ctx, nodeID, map[string]string{"region": "test"}, "readonly", []string{"ocserv.status.read"})
	if bindingErr != nil {
		t.Fatal(bindingErr)
	}
	approvalID, identityID, sessionID := approvedMetadata(t, pool, workspaceID, nodeID, "node.approve", approvalHash, approvalSummary)
	if _, err := service.Approve(ctx, Approval{NodeID: nodeID, Labels: map[string]string{"region": "changed"}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: identityID.String(), ApprovalID: approvalID, IdentityID: identityID, SessionID: sessionID, Reason: "tampered approval content", RequestID: uuid.Must(uuid.NewV7()).String()}); !errors.Is(err, approvalstore.ErrNotReady) {
		t.Fatalf("changed node approval content err=%v", err)
	}
	trust, err := service.Approve(ctx, Approval{NodeID: nodeID, Labels: map[string]string{"region": "test"}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: identityID.String(), ApprovalID: approvalID, IdentityID: identityID, SessionID: sessionID, Reason: "approve fixture", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("approved consumed-token replay error = %v", err)
	}
	permitted, err = service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/enroll/1"})
	if err != nil || !permitted {
		t.Fatalf("active enrollment recovery permitted=%v err=%v", permitted, err)
	}
	handshake := &agentv1.SessionHandshake{ProtocolMajor: 1, ProtocolMinor: 0, AgentVersion: "test", NodeId: nodeID[:], EndpointId: endpoint, Capabilities: []string{"ocserv.status.read"}, OsRelease: "test", BootId: "boot", AgentInstanceId: uuidBytes(), MaxMessageSize: 1024, Time: timestamppb.Now(), Nonce: make([]byte, 16), SealingKeys: enrollmentSealingKeys()}
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
	handshake.SealingKeys = nil
	handshake.Capabilities = []string{"ocserv.status.read", "ocserv.users.write"}
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || !slices.Equal(response.GetNegotiatedCapabilities(), []string{"ocserv.status.read"}) || response.GetSessionGrant() == nil {
		t.Fatalf("legacy sealing-key rollback authorization = %v, %v", response, err)
	}
	handshake.SealingKeys = enrollmentSealingKeys()
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
	revokeHash, revokeSummary := approvalstore.GenericBinding("node.revoke", "node", nodeID)
	revokeApprovalID, revokeIdentityID, revokeSessionID := approvedMetadata(t, pool, workspaceID, nodeID, "node.revoke", revokeHash, revokeSummary)
	if _, err := service.Revoke(ctx, Revocation{NodeID: trust.NodeID, ActorID: revokeIdentityID.String(), ApprovalID: revokeApprovalID, IdentityID: revokeIdentityID, SessionID: revokeSessionID, Reason: "revoke fixture", RequestID: uuid.Must(uuid.NewV7()).String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Revoke(ctx, Revocation{NodeID: trust.NodeID, ActorID: revokeIdentityID.String(), ApprovalID: uuid.Must(uuid.NewV7()), IdentityID: revokeIdentityID, SessionID: revokeSessionID, Reason: "invalid retry revocation", RequestID: uuid.Must(uuid.NewV7()).String()}); !errors.Is(err, approvalstore.ErrNotReady) {
		t.Fatalf("revoked node with unrelated credential error = %v", err)
	}
	if _, err := service.Revoke(ctx, Revocation{NodeID: trust.NodeID, ActorID: revokeIdentityID.String(), ApprovalID: revokeApprovalID, IdentityID: revokeIdentityID, SessionID: revokeSessionID, Reason: "retry revocation", RequestID: uuid.Must(uuid.NewV7()).String()}); err != nil {
		t.Fatalf("revoked node retry error = %v", err)
	}
	transport := &failingTrustTransport{updateFailures: 1, closeFailures: 1}
	worker, err := NewTrustConvergenceWorker(pool, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if worked, runErr := worker.RunOnce(ctx); !worked || runErr == nil {
		t.Fatalf("failed transport update was not retained for retry: worked=%v err=%v", worked, runErr)
	}
	if transport.closeCalls != 0 {
		t.Fatal("node close ran before the exact trust update was acknowledged")
	}
	var updateApplied, closeApplied bool
	if err := pool.QueryRow(ctx, `SELECT update_applied,close_applied FROM node_trust_convergence WHERE node_id=$1`, nodeID).Scan(&updateApplied, &closeApplied); err != nil || updateApplied || closeApplied {
		t.Fatalf("failed trust convergence state update=%v close=%v err=%v", updateApplied, closeApplied, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_trust_convergence SET available_at=now() WHERE node_id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	if worked, runErr := worker.RunOnce(ctx); !worked || runErr == nil {
		t.Fatalf("failed node close was not retained for retry: worked=%v err=%v", worked, runErr)
	}
	if transport.updateCalls != 2 || transport.closeCalls != 1 {
		t.Fatalf("transport convergence calls update=%d close=%d", transport.updateCalls, transport.closeCalls)
	}
	if err := pool.QueryRow(ctx, `SELECT update_applied,close_applied FROM node_trust_convergence WHERE node_id=$1`, nodeID).Scan(&updateApplied, &closeApplied); err != nil || !updateApplied || closeApplied {
		t.Fatalf("failed close convergence state update=%v close=%v err=%v", updateApplied, closeApplied, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_trust_convergence SET available_at=now() WHERE node_id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	if worked, runErr := worker.RunOnce(ctx); !worked || runErr != nil {
		t.Fatalf("trust convergence retry worked=%v err=%v", worked, runErr)
	}
	if transport.updateCalls != 2 || transport.closeCalls != 2 {
		t.Fatalf("transport convergence retry calls update=%d close=%d", transport.updateCalls, transport.closeCalls)
	}
	if err := pool.QueryRow(ctx, `SELECT update_applied,close_applied FROM node_trust_convergence WHERE node_id=$1`, nodeID).Scan(&updateApplied, &closeApplied); err != nil || !updateApplied || !closeApplied {
		t.Fatalf("completed trust convergence state update=%v close=%v err=%v", updateApplied, closeApplied, err)
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
	endpoint := endpointFixture(9)
	token := createToken(t, service, workspaceID, endpoint)
	substitute := endpointFixture(10)
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

func TestDirectTrustCallerCannotForgeEndpointProofIntegration(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Direct trust proof',$2,now(),now())`, workspaceID, "direct-trust-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	defer cleanupWorkspace(ctx, pool, workspaceID)

	service := newTestService(t, pool, "", "test")
	expectedEndpoint := endpointFixture(12)
	forgerEndpoint := endpointFixture(13)
	token := createToken(t, service, workspaceID, expectedEndpoint)
	forged := enrollmentRequest(token.Value, expectedEndpoint)
	canonical, err := EnrollmentCanonicalV1(forged)
	if err != nil {
		t.Fatal(err)
	}
	forgerKey, ok := endpointPrivateKeys.Load(string(forgerEndpoint))
	if !ok {
		t.Fatal("missing forger key fixture")
	}
	forged.Proof.Signature = ed25519.Sign(forgerKey.(ed25519.PrivateKey), canonical)
	if _, err := service.Enroll(ctx, forged); !errors.Is(err, ErrEndpointProof) {
		t.Fatalf("direct trust caller forged endpoint error = %v", err)
	}
	var consumedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT consumed_at FROM enrollment_tokens WHERE id=$1`, token.ID).Scan(&consumedAt); err != nil || consumedAt != nil {
		t.Fatalf("forged enrollment consumed token at %v, err=%v", consumedAt, err)
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
	endpoint := endpointFixture(11)
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

func TestExistingActiveNodeBindsSealingKeysOnceIntegration(t *testing.T) {
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
	nodeID := uuid.Must(uuid.NewV7())
	endpoint := endpointFixture(12)
	for _, setup := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ($1,'Existing sealing migration',$2,now(),now())`, []any{workspaceID, "existing-seal-" + workspaceID.String()}},
		{`INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ($1,$2,'existing-node','active',now(),now())`, []any{nodeID, workspaceID}},
		{`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, []any{nodeID, endpoint}},
		{`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.status.read',true)`, []any{nodeID}},
	} {
		if _, err = pool.Exec(ctx, setup.query, setup.args...); err != nil {
			t.Fatal(err)
		}
	}
	defer cleanupWorkspace(ctx, pool, workspaceID)
	service := newTestService(t, pool, "", "test")
	token, err := service.CreateToken(ctx, TokenSpec{WorkspaceID: workspaceID, Environment: "test", ExpectedNodeName: "existing-node", ExpectedEndpointID: endpoint, ActorID: "integration", Reason: "bind purpose-specific sealing keys", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	request := enrollmentRequest(token.Value, endpoint)
	response, err := service.Enroll(ctx, request)
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || !bytes.Equal(response.GetNodeId(), nodeID[:]) {
		t.Fatalf("existing node sealing enrollment response=%v err=%v", response, err)
	}
	var keyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM node_sealing_keys WHERE node_id=$1`, nodeID).Scan(&keyCount); err != nil || keyCount != 2 {
		t.Fatalf("existing node sealing key count=%d err=%v", keyCount, err)
	}
	if _, err := service.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("existing node consumed-token replay err=%v", err)
	}
	second, err := service.CreateToken(ctx, TokenSpec{WorkspaceID: workspaceID, Environment: "test", ExpectedNodeName: "existing-node", ExpectedEndpointID: endpoint, ActorID: "integration", Reason: "must not replace sealing keys", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enroll(ctx, enrollmentRequest(second.Value, endpoint)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second sealing key binding err=%v", err)
	}
	var consumedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT consumed_at FROM enrollment_tokens WHERE id=$1`, second.ID).Scan(&consumedAt); err != nil || consumedAt != nil {
		t.Fatalf("replacement token consumed_at=%v err=%v", consumedAt, err)
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
	request := &agentv1.EnrollRequest{Token: token, EndpointId: endpoint, AgentVersion: "test", OsRelease: "test", OcservVersion: "test", BootId: "boot", AgentInstanceId: uuidBytes(), Capabilities: []string{"ocserv.status.read"}, Environment: "test", Nonce: make([]byte, 16), Time: timestamppb.Now(), EnrollmentProtocolMajor: EnrollmentProtocolMajor, EnrollmentProtocolMinor: EnrollmentProtocolMinor, SealingKeys: enrollmentSealingKeys()}
	privateKey, ok := endpointPrivateKeys.Load(string(endpoint))
	if !ok {
		panic("missing enrollment endpoint private key fixture")
	}
	canonical, err := EnrollmentCanonicalV1(request)
	if err != nil {
		panic(err)
	}
	request.Proof = &agentv1.EnrollmentProofV1{Version: EnrollmentProofVersionV1, Signature: ed25519.Sign(privateKey.(ed25519.PrivateKey), canonical)}
	return request
}

func enrollmentSealingKeys() []*agentv1.SealingKeyDescriptorV1 {
	return []*agentv1.SealingKeyDescriptorV1{
		{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, KeyId: "fixture-user-key-v1", PublicKeySha256: bytes.Repeat([]byte{0x11}, 32)},
		{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: "fixture-p12-key-v1", PublicKeySha256: bytes.Repeat([]byte{0x22}, 32)},
	}
}

var endpointPrivateKeys sync.Map

type failingTrustTransport struct {
	updateFailures int
	closeFailures  int
	updateCalls    int
	closeCalls     int
}

func (transport *failingTrustTransport) UpdateNodeTrust(_ context.Context, _, _ []byte, _ transportv1.NodeTrustState, _ string, _ uint64) error {
	transport.updateCalls++
	if transport.updateFailures > 0 {
		transport.updateFailures--
		return errors.New("injected trust transport failure")
	}
	return nil
}

func (transport *failingTrustTransport) CloseNode(_ context.Context, _ []byte, _ string) error {
	transport.closeCalls++
	if transport.closeFailures > 0 {
		transport.closeFailures--
		return errors.New("injected node close failure")
	}
	return nil
}

func endpointFixture(seed byte) []byte {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	publicKey := slices.Clone(privateKey.Public().(ed25519.PublicKey))
	endpointPrivateKeys.Store(string(publicKey), privateKey)
	return publicKey
}

func uuidBytes() []byte {
	id := uuid.Must(uuid.NewV7())
	return id[:]
}

func cleanupWorkspace(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID) {
	_, _ = pool.Exec(ctx, `DELETE FROM node_trust_convergence WHERE node_id IN (SELECT id FROM nodes WHERE workspace_id=$1)`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM audit_events WHERE workspace_id=$1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM enrollment_tokens WHERE workspace_id=$1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM node_capabilities WHERE node_id IN (SELECT id FROM nodes WHERE workspace_id=$1)`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM node_endpoint_keys WHERE node_id IN (SELECT id FROM nodes WHERE workspace_id=$1)`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM nodes WHERE workspace_id=$1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspaceID)
}

func approvedMetadata(t *testing.T, pool *pgxpool.Pool, workspaceID, resourceID uuid.UUID, action string, requestHash []byte, requestSummary []byte) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	requesterID, approverID, sessionID, approvalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, requesterID, "requester-"+requesterID.String(), approverID, "approver-"+approverID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, sessionID, requesterID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,$4,'node',$5,'integration','approved',$6,'independent',now()+interval '1 hour',now(),now(),$7,$8)`, approvalID, workspaceID, requesterID, action, resourceID, approverID, requestHash, requestSummary); err != nil {
		t.Fatal(err)
	}
	return approvalID, requesterID, sessionID
}
