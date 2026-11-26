package enrollment

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func enrollmentBackend(t *testing.T) *postgres.Backend {
	t.Helper()
	runtime, _ := testfixture.OpenIsolated(t, t, "enrollment")
	return postgres.WrapPool(runtime)
}

func TestEnrollmentBackendIntegration(t *testing.T) {
	b := enrollmentBackend(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := b.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	workspace := uuid.New()
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'enrollment',$2,now(),now())`, workspace, workspace.String())
	signer, err := commandauth.NewRandomSigner()
	testutil.NoError(t, err)
	s := New(b, "", "test", signer)
	endpoint := endpointFixture(101)
	readOnly := []string{"ocserv.config_fingerprint.read", "ocserv.ip_bans.read", "ocserv.sessions.read", "ocserv.status.read", "ocserv.version.read"}
	capabilities := append(slices.Clone(readOnly), "ocserv.user.manage", "future.feature.read")
	token := createToken(t, s, workspace, endpoint)
	request := enrollmentRequestCapabilities(token.Value, endpoint, capabilities)
	if err := s.ValidateEnrollment(ctx, request); err != nil {
		t.Fatal(err)
	}
	tampered := enrollmentRequestCapabilities(token.Value, endpoint, capabilities)
	tampered.Proof.Signature[0] ^= 1
	if err := s.ValidateEnrollment(ctx, tampered); !errors.Is(err, ErrEndpointProof) {
		t.Fatal("forged proof", err)
	}
	var wg sync.WaitGroup
	type enrollmentResult struct {
		response *agentv1.EnrollResponse
		err      error
	}
	results := make(chan enrollmentResult, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Enroll(ctx, request)
			results <- enrollmentResult{v, err}
		}()
	}
	wg.Wait()
	var node uuid.UUID
	successes := 0
	for range 8 {
		result := <-results
		v, err := result.response, result.err
		if v != nil {
			if v.Result != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED {
				t.Fatal(v)
			}
			copy(node[:], v.NodeId)
			successes++
		}
		if err != nil && !errors.Is(err, ErrEndpointMismatch) {
			t.Fatal(err)
		}
	}
	if successes != 8 || node == uuid.Nil {
		t.Fatal("bootstrap enrollment", successes)
	}
	if err := s.ValidateEnrollment(ctx, request); err != nil {
		t.Fatal("consumed active bootstrap token replay rejected", err)
	}
	if allowed, err := s.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"}); err != nil || !allowed {
		t.Fatal("active agent not permitted", allowed, err)
	}
	requester, session := testfixture.InsertWorkspaceAdminSession(t, ctx, b.Pool(), workspace, "enrollment")
	exported, err := ExportSealingBinding(ctx, b, workspace, node, hex.EncodeToString(endpoint))
	if err != nil {
		t.Fatal("active sealing export", err)
	}
	var binding struct {
		NodeID uuid.UUID `json:"node_id"`
		Keys   []struct {
			Digest string `json:"public_key_sha256"`
		} `json:"keys"`
	}
	if json.Unmarshal(exported, &binding) != nil || binding.NodeID != node || len(binding.Keys) != 2 {
		t.Fatal("invalid exported binding")
	}
	for _, invalid := range []struct {
		workspace, node uuid.UUID
		endpoint        string
	}{
		{uuid.New(), node, hex.EncodeToString(endpoint)},
		{workspace, uuid.New(), hex.EncodeToString(endpoint)},
		{workspace, node, strings.Repeat("a", 64)},
	} {
		if _, err := ExportSealingBinding(ctx, b, invalid.workspace, invalid.node, invalid.endpoint); err == nil {
			t.Fatal("wrong export identity accepted")
		}
	}
	t.Run("legacy-capability-policy", func(t *testing.T) {
		t.Run("grantless-1.0-rejected", func(t *testing.T) {
			handshake := &agentv1.SessionHandshake{ProtocolMajor: ProtocolMajor, ProtocolMinor: 0, AgentVersion: "compatibility-fixture", NodeId: node[:], EndpointId: endpoint, Capabilities: capabilities, MaxMessageSize: 1024, Time: timestamppb.Now(), SealingKeys: enrollmentSealingKeys()}
			connectionID := uuid.Must(uuid.NewV7())
			response, err := s.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake, ConnectionId: connectionID[:]})
			if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_INCOMPATIBLE_PROTOCOL || len(response.GetNegotiatedCapabilities()) != 0 || response.GetSessionGrant() != nil {
				t.Fatalf("protocol 1.0 authorization = %v, %v; want INCOMPATIBLE_PROTOCOL", response, err)
			}
		})
		for _, test := range []struct {
			name    string
			sealing bool
			want    []string
		}{
			{"unsealed-1.1", false, readOnly},
			{"authorized-1.1", true, normalizedCapabilities(capabilities)},
		} {
			t.Run(test.name, func(t *testing.T) {
				handshake := &agentv1.SessionHandshake{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, AgentVersion: "compatibility-fixture", NodeId: node[:], EndpointId: endpoint, Capabilities: capabilities, MaxMessageSize: 1024, Time: timestamppb.Now()}
				if test.sealing {
					handshake.SealingKeys = enrollmentSealingKeys()
				}
				connectionID := uuid.Must(uuid.NewV7())
				response, err := s.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake, ConnectionId: connectionID[:]})
				if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || !slices.Equal(response.GetNegotiatedCapabilities(), test.want) || response.GetSessionGrant() == nil {
					t.Fatalf("compatibility authorization = %v, %v; want capabilities %v", response, err, test.want)
				}
			})
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := ownersession.NewManager(b, logger)
	testutil.NoError(t, err)
	s.ownerSessions = manager
	handshake := &agentv1.SessionHandshake{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, AgentVersion: "test", NodeId: node[:], EndpointId: endpoint, Capabilities: capabilities, MaxMessageSize: 1024, Time: timestamppb.Now(), SealingKeys: enrollmentSealingKeys()}
	v4 := uuid.New()
	for _, invalid := range [][]byte{nil, v4[:]} {
		response, err := s.AuthorizeSession(ctx, &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake, ConnectionId: invalid})
		if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED || response.GetSessionGrant() != nil {
			t.Fatalf("invalid connection ID authorization = %v, %v; want REVOKED", response, err)
		}
	}
	connectionID := uuid.Must(uuid.NewV7())
	authorize := &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake, ConnectionId: connectionID[:]}
	response, err := s.AuthorizeSession(ctx, authorize)
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || response.GetSessionGrant() == nil || response.GetSessionGrant().GetSessionEpoch() == 0 || !slices.Equal(response.NegotiatedCapabilities, normalizedCapabilities(capabilities)) {
		t.Fatal("fenced session", response, err)
	}
	firstEpoch := response.GetSessionGrant().GetSessionEpoch()
	successor, err := ownersession.NewManager(b, logger)
	testutil.NoError(t, err)
	// A restarted Controller must release the term using transportd's identity.
	testutil.NoError(t, successor.CloseSession(ctx, node, connectionID, firstEpoch))
	state, err := connectionowner.ReadState(ctx, b, node)
	testutil.NoError(t, err)
	if state.ConnectionID != connectionID || uint64(state.Epoch) != firstEpoch || state.Active {
		t.Fatalf("transportd disconnect did not release its exact owner term: %+v", state)
	}
	s.ownerSessions = successor
	nextConnectionID := uuid.Must(uuid.NewV7())
	authorize.ConnectionId = nextConnectionID[:]
	response, err = s.AuthorizeSession(ctx, authorize)
	if err != nil || response.GetSessionGrant() == nil || response.GetSessionGrant().GetSessionEpoch() <= firstEpoch {
		t.Fatal("reopened session did not advance session_epoch", response, err)
	}
	revocation := Revocation{NodeID: node, ActorID: requester.String(), IdentityID: requester, SessionID: session, Reason: "revoke", RequestID: uuid.NewString()}
	locked, lockedStore, err := s.begin(ctx, database.RepeatableRead)
	testutil.NoError(t, err)
	if _, err := lockedStore.NodeByEndpoint(ctx, endpoint, uuid.Nil, enrollmentstore.ForShare); err != nil {
		rollback(locked)
		t.Fatal(err)
	}
	bounded, cancelLock := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = s.Revoke(bounded, revocation)
	cancelLock()
	rollback(locked)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("revocation escaped authority share lock", err)
	}
	revoked, err := s.Revoke(ctx, revocation)
	if err != nil || revoked.Revision != 3 {
		t.Fatal("revoke", revoked, err)
	}
	if _, err := ExportSealingBinding(ctx, b, workspace, node, hex.EncodeToString(endpoint)); err == nil {
		t.Fatal("revoked enrollment exported")
	}
	if replay, err := s.Revoke(ctx, revocation); err != nil || replay.Revision != revoked.Revision {
		t.Fatal("revoke replay", replay, err)
	}
	unrelated := revocation
	unrelated.IdentityID = uuid.Must(uuid.NewV7())
	if _, err := s.Revoke(ctx, unrelated); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("unrelated identity authorized revoked-node replay", err)
	}
	for _, alpn := range []string{"ocserv-platform/enroll/1", "ocserv-platform/agent/1"} {
		if permitted, err := s.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: alpn}); err != nil || permitted {
			t.Fatal("revoked endpoint permitted", alpn, permitted, err)
		}
	}
	if response, err := s.AuthorizeSession(ctx, authorize); err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_REVOKED {
		t.Fatal("revoked session", response, err)
	}
	bindings, err := s.ListNodeTrust(ctx)
	if err != nil || !slices.ContainsFunc(bindings, func(v *transportv1.NodeTrustBinding) bool {
		return bytes.Equal(v.NodeId, node[:]) && v.State == transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED && v.Revision == revoked.Revision
	}) {
		t.Fatal("trust snapshot", bindings, err)
	}
	if verified, err := audit.NewManager(b, nil).Verify(ctx, workspace); err != nil || !verified.Valid || verified.Events == 0 {
		t.Fatal("enrollment audit chain", verified, err)
	}

	bootstrapEndpoint := endpointFixture(102)
	bootstrap := createToken(t, s, workspace, bootstrapEndpoint)
	wrongEndpoint := enrollmentRequest(bootstrap.Value, endpointFixture(103))
	if err := s.ValidateEnrollment(ctx, wrongEndpoint); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("prebound token validated another endpoint", err)
	}
	if _, err := s.Enroll(ctx, wrongEndpoint); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("prebound token consumed by another endpoint", err)
	}
	bootstrapRequest := enrollmentRequest(bootstrap.Value, bootstrapEndpoint)
	now := time.Now().UTC()
	created, past, future := now.Add(-2*time.Hour), now.Add(-time.Hour), now.Add(time.Hour)
	exec(`UPDATE node_bootstrap_tokens SET created_at=$1,expires_at=$2 WHERE id=$3`, created, future, bootstrap.ID)
	if err := s.ValidateEnrollment(ctx, bootstrapRequest); err != nil {
		t.Fatal("bootstrap expiry", err)
	}
	bootstrapped, err := s.Enroll(ctx, bootstrapRequest)
	testutil.NoError(t, err)
	exec(`UPDATE node_bootstrap_tokens SET consumed_at=$1,expires_at=$2 WHERE id=$3`, future, past, bootstrap.ID)
	if replay, err := s.Enroll(ctx, bootstrapRequest); err != nil || !bytes.Equal(replay.GetNodeId(), bootstrapped.GetNodeId()) {
		t.Fatal("bound bootstrap retry with expired token", replay, err)
	}
	if _, err := s.Enroll(ctx, enrollmentRequest(bootstrap.Value, endpointFixture(103))); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("bootstrap endpoint substitution", err)
	}
}
