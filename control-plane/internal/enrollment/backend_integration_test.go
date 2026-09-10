package enrollment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func enrollmentBackend(t *testing.T) database.Backend {
	t.Helper()
	ctx := context.Background()
	if dsn := os.Getenv("PR02_DSN"); dsn != "" {
		options := mysql.Options{Engine: mysql.Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
		admin, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		name := "pr02_enrollment_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE `"+name+"`") })
		if _, err := admin.Exec(ctx, "GRANT ALL ON `"+name+"`.* TO 'ocservia_owner'@'%' WITH GRANT OPTION"); err != nil {
			t.Fatal(err)
		}
		cfg, err := driver.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName, cfg.User, cfg.Passwd = name, "ocservia_owner", "pr02-owner-test-only"
		options.DSN = cfg.FormatDSN()
		owner, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Close() })
		if err := owner.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := owner.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
		options.DSN = cfg.FormatDSN()
		backend, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backend.Close() })
		return backend
	}
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL or PR02 backend required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.WrapPool(pool)
}

func TestEnrollmentBackendIntegration(t *testing.T) {
	b := enrollmentBackend(t)
	ctx := context.Background()
	_, my := b.(*mysql.Backend)
	exec := func(pg, sql string, args ...any) {
		t.Helper()
		if my {
			pg = sql
			for i, v := range args {
				if id, ok := v.(uuid.UUID); ok {
					args[i] = mysql.UUIDBytes(id)
				}
			}
		}
		if _, err := b.Exec(ctx, pg, args...); err != nil {
			t.Fatal(err)
		}
	}
	workspace := uuid.New()
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'enrollment',$2,now(),now())`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'enrollment',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, workspace, workspace.String())
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	s := NewBackend(b, "", "test", signer)
	endpoint := endpointFixture(101)
	capabilities := []string{"ocserv.status.read", "ocserv.user.manage", ownersession.FencingCapability}
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
			if v.Result != agentv1.HandshakeResult_HANDSHAKE_RESULT_PENDING_APPROVAL {
				t.Fatal(v)
			}
			copy(node[:], v.NodeId)
			successes++
		}
		if err != nil && !errors.Is(err, ErrInvalidToken) {
			t.Fatal(err)
		}
	}
	if successes != 1 || node == uuid.Nil {
		t.Fatal("one-time enrollment", successes)
	}
	if err := s.ValidateEnrollment(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("consumed token validated", err)
	}
	if allowed, err := s.CheckEndpoint(ctx, &transportv1.CheckEndpointRequest{EndpointId: endpoint, Alpn: "ocserv-platform/agent/1"}); err != nil || allowed {
		t.Fatal("pending agent permitted", allowed, err)
	}
	at, _ := value.FromTime(time.Now().UTC())
	expires, _ := at.Add(time.Hour)
	requester, approver, session, approverSession := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{requester, approver} {
		exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'enrollment',$2,$3,$4)`, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'enrollment',?,?,?)`, id, id.String(), at, at)
		exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at)VALUES($1,$2,$3,'PlatformAdmin','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at)VALUES(?,?,?,'PlatformAdmin','workspace',?)`, uuid.New(), id, workspace, at)
	}
	for _, pair := range [][2]uuid.UUID{{session, requester}, {approverSession, approver}} {
		exec(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES($1,$2,$3,$4)`, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, pair[0], pair[1], expires, at)
	}
	approvalsService := approvals.NewBackend(b)
	approveRequest := func(action string, hash, summary []byte) uuid.UUID {
		t.Helper()
		v, err := approvalsService.Create(ctx, approvals.Request{WorkspaceID: workspace, RequesterID: requester, ResourceID: node, Action: action, ResourceType: "node", Reason: "enrollment", TTL: time.Hour, SessionID: session, RequestID: uuid.NewString(), RequestHash: hash, RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspace, Type: "node", ID: node}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := approvalsService.Approve(ctx, approvals.Decision{ApprovalID: v.ID, ApproverID: approver, SessionID: approverSession, Reason: "reviewed", RequestID: uuid.NewString(), ExpectedRequestHash: v.RequestHash}); err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	_, hash, summary, err := s.ApprovalBinding(ctx, node, nil, "standard", capabilities)
	if err != nil {
		t.Fatal(err)
	}
	approval := Approval{NodeID: node, Policy: "standard", Capabilities: capabilities, ActorID: requester.String(), IdentityID: requester, SessionID: session, ApprovalID: approveRequest("node.approve", hash, summary), Reason: "approve", RequestID: uuid.NewString()}
	changed := approval
	changed.Policy = "changed"
	if _, err := s.Approve(ctx, changed); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatal("approval content substitution", err)
	}
	trust, err := s.Approve(ctx, approval)
	if err != nil || trust.Revision == 0 {
		t.Fatal(trust, err)
	}
	if replay, err := s.Approve(ctx, approval); err != nil || replay.Revision != trust.Revision {
		t.Fatal("approval replay", replay, err)
	}
	if _, _, _, err := s.ApprovalBinding(ctx, node, nil, "standard", capabilities); !errors.Is(err, database.ErrNotFound) {
		t.Fatal("active approval binding")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := ownersession.NewManagerBackend(b, signer, &recordingRegistrar{}, 30*time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	s.ownerSessions = manager
	handshake := &agentv1.SessionHandshake{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, AgentVersion: "test", NodeId: node[:], EndpointId: endpoint, Capabilities: capabilities, MaxMessageSize: 1024, Time: timestamppb.Now(), SealingKeys: enrollmentSealingKeys()}
	authorize := &transportv1.AuthorizeSessionRequest{RemoteEndpointId: endpoint, Handshake: handshake}
	response, err := s.AuthorizeSession(ctx, authorize)
	if err != nil || response.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || response.GetConnectionFence() == nil || response.GetSessionGrant() == nil || !slices.Equal(response.NegotiatedCapabilities, normalizedCapabilities(capabilities)) {
		t.Fatal("fenced session", response, err)
	}
	fence := response.ConnectionFence
	connection, err := fixed16(fence.GetConnectionId())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseSession(ctx, [16]byte(node), connection, int64(fence.GetOwnerEpoch())); err != nil {
		t.Fatal(err)
	}
	// A grant whose authority transaction fails must release the exact newly
	// opened owner term, including on a backend without pgx transactions.
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	failingManager, err := ownersession.NewManagerBackend(b, signer, &cancellingRegistrar{cancelled: cancel}, 30*time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	s.ownerSessions = failingManager
	if response, err := s.AuthorizeSession(cancelled, authorize); err == nil || response != nil {
		t.Fatal("cancelled authorization granted", response, err)
	}
	successor, err := ownersession.NewManagerBackend(b, signer, &recordingRegistrar{}, 30*time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	s.ownerSessions = successor
	response, err = s.AuthorizeSession(ctx, authorize)
	if err != nil || response.GetConnectionFence() == nil || response.ConnectionFence.OwnerEpoch <= fence.OwnerEpoch {
		t.Fatal("failed commit leaked owner lease", response, err)
	}
	fence = response.ConnectionFence
	connection, _ = fixed16(fence.ConnectionId)
	defer successor.CloseSession(context.Background(), [16]byte(node), connection, int64(fence.OwnerEpoch))
	revokeHash, revokeSummary := approvals.GenericBinding("node.revoke", "node", node)
	revocation := Revocation{NodeID: node, ActorID: requester.String(), IdentityID: requester, SessionID: session, ApprovalID: approveRequest("node.revoke", revokeHash, revokeSummary), Reason: "revoke", RequestID: uuid.NewString()}
	locked, lockedStore, err := s.begin(ctx, database.RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil || revoked.Revision != trust.Revision+1 {
		t.Fatal("revoke", revoked, err)
	}
	if replay, err := s.Revoke(ctx, revocation); err != nil || replay.Revision != revoked.Revision {
		t.Fatal("revoke replay", replay, err)
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
	if verified, err := audit.NewBackendManager(b, nil).Verify(ctx, workspace); err != nil || !verified.Valid || verified.Events == 0 {
		t.Fatal("enrollment audit chain", verified, err)
	}

	bootstrap, err := s.CreateBootstrapToken(ctx, BootstrapTokenSpec{WorkspaceID: workspace, Environment: "test", ActorID: "operator", Reason: "bootstrap", RequestID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapEndpoint := endpointFixture(102)
	bootstrapRequest := enrollmentRequest(bootstrap.Value, bootstrapEndpoint)
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	exec(`UPDATE node_bootstrap_tokens SET created_at=$1,expires_at=$2 WHERE id=$3`, `UPDATE node_bootstrap_tokens SET created_at=?,expires_at=? WHERE id=?`, negative, positive, bootstrap.ID)
	if err := s.ValidateEnrollment(ctx, bootstrapRequest); err != nil {
		t.Fatal("logical bootstrap expiry", err)
	}
	bootstrapped, err := s.Enroll(ctx, bootstrapRequest)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE node_bootstrap_tokens SET consumed_at=$1,expires_at=$2 WHERE id=$3`, `UPDATE node_bootstrap_tokens SET consumed_at=?,expires_at=? WHERE id=?`, positive, value.Timestamp{Valid: true, Micros: value.MinTimestamp}, bootstrap.ID)
	if replay, err := s.Enroll(ctx, bootstrapRequest); err != nil || !bytes.Equal(replay.GetNodeId(), bootstrapped.GetNodeId()) {
		t.Fatal("bound bootstrap retry with expired token", replay, err)
	}
	if _, err := s.Enroll(ctx, enrollmentRequest(bootstrap.Value, endpointFixture(103))); !errors.Is(err, ErrEndpointMismatch) {
		t.Fatal("bootstrap endpoint substitution", err)
	}
	wideEndpoint := endpointFixture(104)
	wide := createToken(t, s, workspace, wideEndpoint)
	exec(`UPDATE enrollment_tokens SET created_at=$1,expires_at=$2 WHERE id=$3`, `UPDATE enrollment_tokens SET created_at=?,expires_at=? WHERE id=?`, negative, value.Timestamp{Valid: true, Micros: value.EndTimestamp - 1}, wide.ID)
	if _, err := s.Enroll(ctx, enrollmentRequest(wide.Value, wideEndpoint)); err != nil {
		t.Fatal("full finite token expiry", err)
	}
	legacy, legacyEndpoint := uuid.New(), endpointFixture(105)
	legacyName := "legacy-" + legacy.String()
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,$3,'pending',now(),now())`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'pending',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, legacy, workspace, legacyName)
	legacyToken, err := s.CreateToken(ctx, TokenSpec{WorkspaceID: workspace, Environment: "test", ExpectedNodeName: legacyName, ExpectedEndpointID: legacyEndpoint, ActorID: "operator", Reason: "legacy enrollment", RequestID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Enroll(ctx, enrollmentRequest(legacyToken.Value, legacyEndpoint)); err != nil || !bytes.Equal(v.GetNodeId(), legacy[:]) {
		t.Fatal("legacy pending node was not reused", v, err)
	}
	retrofit, retrofitEndpoint := uuid.New(), endpointFixture(106)
	if err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		store, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		if err := store.InsertNode(ctx, retrofit, workspace, retrofit.String(), at); err != nil {
			return err
		}
		if err := store.InsertEndpoint(ctx, retrofit, retrofitEndpoint, at); err != nil {
			return err
		}
		if err := store.PutCapability(ctx, retrofit, "ocserv.status.read", true); err != nil {
			return err
		}
		_, err = store.Activate(ctx, retrofit, "{}", "standard", at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retrofitToken := createToken(t, s, workspace, retrofitEndpoint)
	if v, err := s.Enroll(ctx, enrollmentRequest(retrofitToken.Value, retrofitEndpoint)); err != nil || v.GetResult() != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED || !bytes.Equal(v.NodeId, retrofit[:]) {
		t.Fatal("existing node sealing-key binding", v, err)
	}
	duplicate := createToken(t, s, workspace, retrofitEndpoint)
	if _, err := s.Enroll(ctx, enrollmentRequest(duplicate.Value, retrofitEndpoint)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("existing sealing keys were replaced", err)
	}
}
