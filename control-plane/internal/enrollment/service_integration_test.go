package enrollment

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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

	service := New(pool, string(make([]byte, 64)))
	endpoint := make([]byte, 32)
	endpoint[0] = 7
	token := createToken(t, service, workspaceID, endpoint)
	request := enrollmentRequest(token.Value, endpoint)

	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, enrollErr := service.Enroll(ctx, request)
			if enrollErr == nil && response.GetResult() == agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL {
				successes.Add(1)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent token successes = %d", successes.Load())
	}
	if _, err := service.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replay error = %v", err)
	}
	permitted, err := service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"})
	if err != nil || permitted {
		t.Fatalf("pending endpoint permitted=%v err=%v", permitted, err)
	}

	var nodeID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT consumed_node_id FROM enrollment_tokens WHERE id=$1`, token.ID).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	trust, err := service.Approve(ctx, Approval{NodeID: nodeID, Labels: map[string]string{"region": "test"}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: "integration", Reason: "approve fixture", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	handshake := &agentv1.SessionHandshake{ProtocolMajor: 1, ProtocolMinor: 0, AgentVersion: "test", NodeId: nodeID[:], EndpointId: endpoint, Capabilities: []string{"ocserv.status.read"}, OsRelease: "test", BootId: "boot", AgentInstanceId: uuidBytes(), MaxMessageSize: 1024, Time: timestamppb.Now(), Nonce: make([]byte, 16)}
	response, err := service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED {
		t.Fatalf("active authorization = %v, %v", response, err)
	}
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
	if retryTrust, err := service.Approve(ctx, Approval{NodeID: nodeID, Labels: map[string]string{"region": "test"}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: "integration", Reason: "retry approval", RequestID: uuid.Must(uuid.NewV7()).String()}); err != nil || retryTrust.NodeID != nodeID {
		t.Fatalf("offline approval retry = %v, %v", retryTrust, err)
	}
	handshake.Time = timestamppb.New(time.Now().Add(-MaxClockSkew - time.Second))
	response, err = service.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake})
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_CLOCK_SKEW {
		t.Fatalf("clock skew authorization = %v, %v", response, err)
	}
	if _, err := service.Revoke(ctx, Revocation{NodeID: trust.NodeID, ActorID: "integration", Reason: "revoke fixture", RequestID: uuid.Must(uuid.NewV7()).String()}); err != nil {
		t.Fatal(err)
	}
	permitted, err = service.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"})
	if err != nil || permitted {
		t.Fatalf("revoked endpoint permitted=%v err=%v", permitted, err)
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
	service := New(pool, "")
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

func createToken(t *testing.T, service *Service, workspaceID uuid.UUID, endpoint []byte) Token {
	t.Helper()
	token, err := service.CreateToken(context.Background(), TokenSpec{WorkspaceID: workspaceID, Environment: "test", ExpectedEndpointID: endpoint, ActorID: "integration", Reason: "test enrollment", RequestID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	return token
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
