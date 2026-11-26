//go:build integration

package enrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testEndpointAndRequest(t *testing.T, service *Service, workspace uuid.UUID, seed byte) ([]byte, *agentv1.EnrollRequest) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	endpoint := slices.Clone(priv.Public().(ed25519.PublicKey))
	if service.installation.Origin == "" {
		testutil.NoError(t, service.ConfigureInstallation(InstallationTrust{
			Origin: "https://controller.example", RelayURL: "https://relay.example", RelayToken: "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr",
		}))
	}
	pool := service.backend.(*postgres.Backend).Pool()
	identity, session := testfixture.InsertWorkspaceAdminSession(t, context.Background(), pool, workspace, "integration")
	token, err := service.CreateInstallationToken(context.Background(), BootstrapTokenSpec{
		WorkspaceID: workspace, Environment: "test", ExpectedEndpointID: endpoint,
		ActorID: identity.String(), ActorIdentityID: identity, ActorSessionID: session,
		Reason: "test enrollment", RequestID: uuid.Must(uuid.NewV7()).String(),
	})
	testutil.NoError(t, err)
	child, ok := installationChild(token.Value, service.installation.Origin)
	if !ok {
		t.Fatal("failed deriving installation child token")
	}
	sealing := []*agentv1.SealingKeyDescriptorV1{
		{Version: 1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, KeyId: "fixture-user-key-v1", PublicKeySha256: bytes.Repeat([]byte{0x11}, 32)},
		{Version: 1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: "fixture-p12-key-v1", PublicKeySha256: bytes.Repeat([]byte{0x22}, 32)},
	}
	req := &agentv1.EnrollRequest{
		Token: child, EndpointId: endpoint, AgentVersion: "test", OsRelease: "test", OcservVersion: "test",
		BootId: "boot", AgentInstanceId: testfixture.UUIDBytes(), Capabilities: []string{"ocserv.status.read", "ocserv.user.manage"},
		Environment: "test", Nonce: make([]byte, 16), Time: timestamppb.Now(),
		EnrollmentProtocolMajor: EnrollmentProtocolMajor, EnrollmentProtocolMinor: EnrollmentProtocolMinor,
		SealingKeys: sealing,
	}
	canonical, err := EnrollmentCanonicalV1(req)
	testutil.NoError(t, err)
	req.Proof = &agentv1.EnrollmentProofV1{Version: EnrollmentProofVersionV1, Signature: ed25519.Sign(priv, canonical)}
	return endpoint, req
}

func TestEnrollmentBackendIntegration(t *testing.T) {
	runtime, _ := testfixture.OpenIsolated(t, t, "enrollment")
	b := postgres.WrapPool(runtime)
	ctx := context.Background()
	workspace := uuid.New()
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'enrollment',$2,now(),now())`, workspace, workspace.String()); err != nil {
		t.Fatal(err)
	}
	signer, err := commandauth.NewRandomSigner()
	testutil.NoError(t, err)
	s := New(b, "", "test", signer)

	endpoint, req := testEndpointAndRequest(t, s, workspace, 101)
	testutil.NoError(t, s.ValidateEnrollment(ctx, req))

	var wg sync.WaitGroup
	var node uuid.UUID
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := s.Enroll(ctx, req); err == nil && resp.Result == agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED {
				copy(node[:], resp.NodeId)
			}
		}()
	}
	wg.Wait()
	if node == uuid.Nil {
		t.Fatal("concurrent enrollment failed")
	}

	exported, err := ExportSealingBinding(ctx, b, workspace, node, hex.EncodeToString(endpoint))
	testutil.NoError(t, err)
	var binding struct {
		NodeID uuid.UUID `json:"node_id"`
	}
	if json.Unmarshal(exported, &binding) != nil || binding.NodeID != node {
		t.Fatal("invalid exported binding")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := ownersession.NewManager(b, logger)
	testutil.NoError(t, err)
	s.ownerSessions = manager
	connID := uuid.Must(uuid.NewV7())
	handshake := &agentv1.SessionHandshake{
		ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, AgentVersion: "test",
		NodeId: node[:], EndpointId: endpoint, Capabilities: req.Capabilities,
		MaxMessageSize: 1024, Time: timestamppb.Now(), SealingKeys: req.SealingKeys,
	}
	resp, err := s.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake, ConnectionId: connID[:]})
	if err != nil || resp.GetSessionGrant() == nil || resp.GetSessionGrant().GetSessionEpoch() == 0 {
		t.Fatalf("fenced session failed: %v %v", resp, err)
	}
	firstEpoch := resp.GetSessionGrant().GetSessionEpoch()
	testutil.NoError(t, manager.CloseSession(ctx, node, connID, firstEpoch))
	if state, err := connectionowner.ReadState(ctx, b, node); err != nil || state.Active {
		t.Fatalf("close session did not release term: %+v %v", state, err)
	}

	pool := runtime
	requester, session := testfixture.InsertWorkspaceAdminSession(t, ctx, pool, workspace, "revoke")
	revocation := Revocation{NodeID: node, ActorID: requester.String(), IdentityID: requester, SessionID: session, Reason: "revoke", RequestID: uuid.NewString()}
	if _, err := s.Revoke(ctx, revocation); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	unrelated := revocation
	unrelated.IdentityID = uuid.Must(uuid.NewV7())
	if _, err := s.Revoke(ctx, unrelated); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("unrelated identity authorized revoked-node replay: %v", err)
	}
	if permitted, err := s.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"}); err != nil || permitted {
		t.Fatalf("revoked endpoint permitted: %v %v", permitted, err)
	}
}
