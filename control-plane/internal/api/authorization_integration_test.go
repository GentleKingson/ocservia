package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type certificateArtifactFixture struct{ data []byte }

func TestCreateNodeBootstrapTokenIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	owner := testfixture.OpenOwnerPool(t)
	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Bootstrap API',$2,now(),now())`, workspaceID, "bootstrap-api-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := owner.Exec(context.Background(), `BEGIN;
			ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only;
			DELETE FROM audit_events WHERE workspace_id=$1;
			DELETE FROM node_bootstrap_tokens WHERE workspace_id=$1;
			DELETE FROM workspaces WHERE id=$1;
			ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only;
			COMMIT`, pgx.QueryExecModeSimpleProtocol, workspaceID)
		if err != nil {
			t.Errorf("cleanup node bootstrap token fixture: %v", err)
		}
	})
	signer, err := commandauth.NewRandomSigner()
	testutil.NoError(t, err)
	server := &Server{backend: postgres.WrapPool(pool), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), devAuth: true, enrollment: enrollment.NewBackend(postgres.WrapPool(pool), "", "test", signer)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", strings.NewReader(fmt.Sprintf(`{"workspace_id":%q,"environment":"production","reason":"bootstrap API test","expected_endpoint_id":"1111111111111111111111111111111111111111111111111111111111111111"}`, workspaceID.String())))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(context.WithValue(request.Context(), requestIDKey{}, uuid.Must(uuid.NewV7()).String()), principalKey{}, auth.Principal{Subject: "developer", Issuer: "development", BreakGlass: true}))
	response := httptest.NewRecorder()
	server.createNodeBootstrapToken(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("bootstrap token status=%d cache-control=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	var body struct {
		Token string    `json:"token"`
		ID    uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !strings.HasPrefix(body.Token, enrollment.BootstrapTokenPrefix) {
		t.Fatalf("bootstrap token response=%q err=%v", body.Token, err)
	}
	var endpoint string
	if err := pool.QueryRow(ctx, `SELECT encode(expected_endpoint_id,'hex') FROM node_bootstrap_tokens WHERE id=$1`, body.ID).Scan(&endpoint); err != nil || endpoint != strings.Repeat("11", 32) {
		t.Fatalf("endpoint restriction not persisted: %q %v", endpoint, err)
	}

}

func (f certificateArtifactFixture) FetchArtifact(context.Context, *agentv1.ArtifactGrantV1, *agentv1.FenceBindingV2) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (certificateArtifactFixture) ConsumeArtifact(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) error {
	return nil
}

func (certificateArtifactFixture) ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) (bool, error) {
	return true, nil
}

type invalidatingArtifactFixture struct {
	pool       *pgxpool.Pool
	artifactID uuid.UUID
	data       []byte
}

func (f invalidatingArtifactFixture) FetchArtifact(ctx context.Context, _ *agentv1.ArtifactGrantV1, _ *agentv1.FenceBindingV2) (io.ReadCloser, error) {
	if _, err := f.pool.Exec(ctx, `UPDATE artifact_operations SET state='failed',lease_until=NULL WHERE id=$1`, f.artifactID); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (invalidatingArtifactFixture) ConsumeArtifact(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) error {
	return nil
}

func (invalidatingArtifactFixture) ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) (bool, error) {
	return true, nil
}

type terminalArtifactFixture struct {
	pool       *pgxpool.Pool
	artifactID uuid.UUID
	state      string
	data       []byte
}

func (f terminalArtifactFixture) FetchArtifact(context.Context, *agentv1.ArtifactGrantV1, *agentv1.FenceBindingV2) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (f terminalArtifactFixture) ConsumeArtifact(ctx context.Context, _ *agentv1.ArtifactGrantV1, _ []byte, _ int64, _ *agentv1.FenceBindingV2) error {
	command, err := f.pool.Exec(ctx, `UPDATE artifact_operations SET state=$2,lease_until=NULL,active_grant_expires_at=now(),updated_at=now() WHERE id=$1 AND state='consuming'`, f.artifactID, f.state)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return errors.New("terminal artifact race did not win")
	}
	return nil
}

func (terminalArtifactFixture) ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) (bool, error) {
	return true, nil
}

type failingArtifactWriter struct {
	header http.Header
}

func (w *failingArtifactWriter) Header() http.Header { return w.header }
func (*failingArtifactWriter) WriteHeader(int)       {}
func (*failingArtifactWriter) Write(value []byte) (int, error) {
	return len(value) / 2, io.ErrUnexpectedEOF
}

func TestCertificateRoutesUseNodeScopedAuthorizationIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	workspaceID, managerID, outsiderID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeA, nodeB, operationID, certificateID, artifactID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	// Only fixture batches use simple protocol; service queries retain the
	// production pool's extended-protocol default.
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'certificate auth',$2,now(),now());
		INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($3,'integration',$4,now(),now()),($5,'integration',$6,now(),now());
		INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($7,$1,'cert-node-a','active',1,now(),now()),($8,$1,'cert-node-b','active',1,now(),now());
		INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,idempotency_key,request_hash,created_at,updated_at) VALUES($9,$1,$7,'succeeded',1,'certificate-auth','certificate-auth',decode(repeat('00',32),'hex'),now(),now());
		INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at) VALUES($10,$1,$7,$9,'node-a.example.test','[]',2048,'csr_pending',now(),now());
		INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,certificate_version,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at) VALUES($11,$1,$7,$10,1,$9,'certificate_p12','pending',decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),now()+interval '10 minutes',now(),now());
		INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$3,now())`, pgx.QueryExecModeSimpleProtocol, workspaceID, "certificate-auth-"+workspaceID.String(), managerID, managerID.String(), outsiderID, outsiderID.String(), nodeA, nodeB, operationID, certificateID, artifactID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace_admins WHERE workspace_id=$1; DELETE FROM artifact_operations WHERE id=$2; DELETE FROM certificates WHERE id=$3; DELETE FROM operations WHERE id=$4; DELETE FROM nodes WHERE workspace_id=$1; DELETE FROM identities WHERE id IN($5,$6); DELETE FROM workspaces WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, workspaceID, artifactID, certificateID, operationID, managerID, outsiderID)
	}()
	artifactData := []byte("encrypted artifact response")
	grantSigner := commandauth.NewSignerFromSeed([32]byte{7})
	authn, err := auth.NewBackend(postgres.WrapPool(pool), auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)
	server := &Server{auth: authn, certificates: certificates.NewBackend(postgres.WrapPool(pool), apiOperationService(pool), nil, nil, certificateArtifactFixture{data: artifactData}, grantSigner)}
	manager := auth.Principal{IdentityID: managerID, SessionID: uuid.Must(uuid.NewV7()), Issuer: "integration"}
	outsider := auth.Principal{IdentityID: outsiderID, SessionID: uuid.Must(uuid.NewV7()), Issuer: "integration"}
	tests := []struct {
		name, method, path, pathKey, pathValue string
		principal                              auth.Principal
		allowed                                bool
	}{
		{"admin lists node A", http.MethodGet, "/api/v1/nodes/" + nodeA.String() + "/certificates", "node_id", nodeA.String(), manager, true},
		{"admin lists node B", http.MethodGet, "/api/v1/nodes/" + nodeB.String() + "/certificates", "node_id", nodeB.String(), manager, true},
		{"admin issues node A", http.MethodPost, "/api/v1/nodes/" + nodeA.String() + "/certificates", "node_id", nodeA.String(), manager, true},
		{"admin creates p12", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":p12", "certificate_action", certificateID.String() + ":p12", manager, true},
		{"admin revokes certificate", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":revoke", "certificate_action", certificateID.String() + ":revoke", manager, true},
		{"admin downloads artifact", http.MethodGet, "/api/v1/artifacts/" + artifactID.String(), "artifact_id", artifactID.String(), manager, true},
		{"outsider cannot list node A", http.MethodGet, "/api/v1/nodes/" + nodeA.String() + "/certificates", "node_id", nodeA.String(), outsider, false},
		{"outsider cannot revoke certificate", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":revoke", "certificate_action", certificateID.String() + ":revoke", outsider, false},
		{"outsider cannot read artifact", http.MethodGet, "/api/v1/artifacts/" + artifactID.String(), "artifact_id", artifactID.String(), outsider, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.SetPathValue(test.pathKey, test.pathValue)
			_, err := server.authorizeRoute(request, test.principal)
			if test.allowed && err != nil {
				t.Fatalf("expected authorization: %v", err)
			}
			if !test.allowed && !errors.Is(err, database.ErrNotFound) {
				t.Fatalf("expected not-found for unauthorized resource, got %v", err)
			}
		})
	}
	token := strings.Repeat("a", 43)
	tokenHash, artifactHash := sha256.Sum256([]byte(token)), sha256.Sum256(artifactData)
	// Download authorization also covers certificates issued before root CSR
	// receipts existed. This fixture does not simulate current CSR issuance.
	if _, err := pool.Exec(ctx, `UPDATE certificates SET state='issued',csr_receipt_legacy=true,certificate_chain_pem=decode(repeat('41',64),'hex'),serial_number='1',not_before=now()-interval '1 minute',not_after=now()+interval '1 hour' WHERE id=$1; UPDATE artifact_operations SET state='ready',token_sha256=$3,content_sha256=$4,content_size=$5 WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, certificateID, artifactID, tokenHash[:], artifactHash[:], len(artifactData)); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+artifactID.String(), nil)
	request.SetPathValue("artifact_id", artifactID.String())
	request.Header.Set("X-Artifact-Token", token)
	request = request.WithContext(context.WithValue(context.WithValue(request.Context(), principalKey{}, manager), requestIDKey{}, "artifact-interruption"))
	response := &failingArtifactWriter{header: make(http.Header)}
	server.downloadArtifact(response, request)
	var artifactState string
	if err := pool.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=$1`, artifactID).Scan(&artifactState); err != nil || artifactState != "consumed" {
		t.Fatalf("one-time download state=%q err=%v", artifactState, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',consumed_at=NULL,content_sha256=$2,content_size=$3 WHERE id=$1`, artifactID, artifactHash[:], len(artifactData)); err != nil {
		t.Fatal(err)
	}
	failureServer := &Server{certificates: certificates.NewBackend(postgres.WrapPool(pool), apiOperationService(pool), nil, nil, invalidatingArtifactFixture{pool: pool, artifactID: artifactID, data: artifactData}, grantSigner)}
	failureRequest := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+artifactID.String(), nil)
	failureRequest.SetPathValue("artifact_id", artifactID.String())
	failureRequest.Header.Set("X-Artifact-Token", token)
	failureRequest = failureRequest.WithContext(context.WithValue(context.WithValue(failureRequest.Context(), principalKey{}, manager), requestIDKey{}, "artifact-completion-failure"))
	failureResponse := httptest.NewRecorder()
	failureServer.downloadArtifact(failureResponse, failureRequest)
	if failureResponse.Code != http.StatusForbidden || failureResponse.Header().Get("Content-Disposition") != "" || failureResponse.Header().Get("Content-Length") != "" || !strings.Contains(failureResponse.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("completion failure status=%d headers=%v body=%s", failureResponse.Code, failureResponse.Header(), failureResponse.Body.String())
	}
	for _, terminalState := range []string{"revoked", "expired"} {
		t.Run("terminal "+terminalState+" prevents delivery", func(t *testing.T) {
			if _, err := pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',consumed_at=NULL,lease_until=NULL,active_grant_id=NULL,active_grant_subject=NULL,active_grant_expires_at=NULL,consume_grant=NULL,consume_sha256=NULL,consume_size=NULL,consume_actor_id=NULL,consume_session_id=NULL,consume_request_id=NULL,content_sha256=$2,content_size=$3 WHERE id=$1`, artifactID, artifactHash[:], len(artifactData)); err != nil {
				t.Fatal(err)
			}
			terminalServer := &Server{certificates: certificates.NewBackend(postgres.WrapPool(pool), apiOperationService(pool), nil, nil, terminalArtifactFixture{pool: pool, artifactID: artifactID, state: terminalState, data: artifactData}, grantSigner)}
			terminalRequest := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+artifactID.String(), nil)
			terminalRequest.SetPathValue("artifact_id", artifactID.String())
			terminalRequest.Header.Set("X-Artifact-Token", token)
			terminalRequest = terminalRequest.WithContext(context.WithValue(context.WithValue(terminalRequest.Context(), principalKey{}, manager), requestIDKey{}, "artifact-terminal-race-"+terminalState))
			terminalResponse := httptest.NewRecorder()
			terminalServer.downloadArtifact(terminalResponse, terminalRequest)
			if terminalResponse.Code != http.StatusForbidden || terminalResponse.Header().Get("Content-Disposition") != "" || terminalResponse.Header().Get("Content-Length") != "" || bytes.Contains(terminalResponse.Body.Bytes(), artifactData) {
				t.Fatalf("terminal race state=%s status=%d headers=%v body=%q", terminalState, terminalResponse.Code, terminalResponse.Header(), terminalResponse.Body.Bytes())
			}
			var retainedState string
			if err := pool.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=$1`, artifactID).Scan(&retainedState); err != nil || retainedState != terminalState {
				t.Fatalf("terminal race retained state=%q err=%v", retainedState, err)
			}
		})
	}
}

func TestSyntheticCommandAuditUsesAuthenticatedOperator(t *testing.T) {
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	owner := testfixture.OpenOwnerPool(t)
	workspaceID, identityID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeID, sessionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'synthetic audit',$2,now(),now());
		INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($3,'integration',$4,now(),now());
		INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($5,$3,now()+interval '1 hour',now());
		INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($6,$1,'audit-node','active',1,now(),now());
		INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$3,now())`,
		pgx.QueryExecModeSimpleProtocol, workspaceID, "synthetic-audit-"+workspaceID.String(), identityID, identityID.String(), sessionID, nodeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := owner.Exec(context.Background(), `BEGIN;
			ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only;
			DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1);
			DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1);
			DELETE FROM commands WHERE workspace_id=$1;
			DELETE FROM operations WHERE workspace_id=$1;
			DELETE FROM audit_events WHERE workspace_id=$1;
			DELETE FROM workspace_admins WHERE workspace_id=$1;
			DELETE FROM nodes WHERE workspace_id=$1;
			DELETE FROM auth_sessions WHERE id=$2;
			DELETE FROM identities WHERE id=$3;
			DELETE FROM workspaces WHERE id=$1;
			ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only;
			COMMIT`, pgx.QueryExecModeSimpleProtocol, workspaceID, sessionID, identityID)
		if err != nil {
			t.Errorf("cleanup synthetic command audit fixture: %v", err)
		}
	})

	authn, err := auth.NewBackend(postgres.WrapPool(pool), auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)
	server := &Server{auth: authn, operations: apiOperationService(pool)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+nodeID.String()+"/synthetic-commands", strings.NewReader(`{"kind":"noop","expected_version":1}`))
	request.SetPathValue("node_id", nodeID.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "authenticated-operator-audit")
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	principal := auth.Principal{IdentityID: identityID, SessionID: sessionID, Subject: identityID.String(), Issuer: "integration"}
	authorized, err := server.authorizeRoute(request, principal)
	if err != nil {
		t.Fatalf("authorize synthetic command: %v", err)
	}
	request = request.WithContext(context.WithValue(authorized, requestIDKey{}, "request-authenticated-operator-audit"))
	response := httptest.NewRecorder()
	server.createSyntheticCommand(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("synthetic command status=%d body=%s", response.Code, response.Body.String())
	}
	var actorID, action, reason string
	var sourceSessionID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT actor_id,action,reason,source_session_id FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspaceID).Scan(&actorID, &action, &reason, &sourceSessionID); err != nil {
		t.Fatal(err)
	}
	if actorID != identityID.String() || sourceSessionID != sessionID || action != "operation.create" || reason != "operator synthetic command" {
		t.Fatalf("synthetic audit actor=%q session=%s action=%q reason=%q", actorID, sourceSessionID, action, reason)
	}
}

func TestBrowserTrustBoundaryBlocksCrossSiteCookieMutations(t *testing.T) {
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	// pgx executes one statement per Exec call, so the fixture and cleanup
	// stay single-statement instead of relying on script-style semicolons.
	fixture := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'csrf boundary',$2,now(),now())`, []any{workspaceID, "csrf-boundary-" + workspaceID.String()}},
		{`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'csrf-node','active',1,now(),now())`, []any{nodeID, workspaceID}},
	}
	for _, statement := range fixture {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		cleanup := []string{
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`,
			`DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`,
			`DELETE FROM security_alerts WHERE source_session_id IN(SELECT s.id FROM auth_sessions s JOIN identities i ON i.id=s.identity_id WHERE i.issuer='break-glass')`,
			`DELETE FROM break_glass_uses WHERE identity_id IN(SELECT id FROM identities WHERE issuer='break-glass')`,
			`DELETE FROM auth_sessions WHERE identity_id IN(SELECT id FROM identities WHERE issuer='break-glass')`,
			`DELETE FROM identities WHERE issuer='break-glass'`,
			`DELETE FROM nodes WHERE workspace_id=$1`,
			`DELETE FROM workspaces WHERE id=$1`,
		}
		for _, statement := range cleanup {
			_, _ = pool.Exec(context.Background(), statement, workspaceID)
		}
	}()

	breakGlassToken := strings.Repeat("break-glass-boundary-token-", 2)
	authService, err := auth.NewBackend(postgres.WrapPool(pool), auth.Config{
		LocalEnabled:      true,
		SessionKey:        make([]byte, 32),
		SessionTTL:        time.Hour,
		BreakGlassEnabled: true, BreakGlassTokenHash: auth.TokenHash(breakGlassToken),
	})
	testutil.NoError(t, err)
	config := testHTTPConfig(false)
	config.BodyLimit = 1024
	server := newTestServer(t, config, postgres.WrapPool(pool), Modules{Operations: apiOperationService(pool)}, Authorization{Authentication: authService})

	do := func(method, path, origin, fetchSite, contentType, cookie, idempotencyKey, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if fetchSite != "" {
			request.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		if cookie != "" {
			request.Header.Set("Cookie", auth.SessionCookieName+"="+cookie)
		}
		if idempotencyKey != "" {
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		return response
	}

	var breakGlassUsesBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM break_glass_uses`).Scan(&breakGlassUsesBefore); err != nil {
		t.Fatal(err)
	}
	sibling := do(http.MethodPost, "/api/v1/auth/break-glass",
		"https://app.example.test", "", "application/json", "", "", `{"token":"`+breakGlassToken+`"}`)
	if sibling.Code != http.StatusForbidden || !strings.Contains(sibling.Body.String(), "https://ocservia.dev/problems/cross-origin-request") {
		t.Fatalf("sibling-origin break-glass status=%d body=%s", sibling.Code, sibling.Body.String())
	}
	var blockedUses int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM break_glass_uses`).Scan(&blockedUses); err != nil || blockedUses != breakGlassUsesBefore {
		t.Fatalf("sibling-origin break-glass reached the database: uses=%d want %d err=%v", blockedUses, breakGlassUsesBefore, err)
	}

	for attempt := 0; attempt < 6; attempt++ {
		invalid := do(http.MethodPost, "/api/v1/auth/break-glass",
			"https://admin.example.test", "", "application/json", "", "", `{"token":"invalid"}`)
		want := http.StatusUnauthorized
		if attempt == 5 {
			want = http.StatusTooManyRequests
		}
		if invalid.Code != want {
			t.Fatalf("invalid emergency attempt=%d status=%d body=%s", attempt, invalid.Code, invalid.Body)
		}
	}
	legitimate := do(http.MethodPost, "/api/v1/auth/break-glass",
		"https://admin.example.test", "", "application/json", "", "", `{"token":"`+breakGlassToken+`"}`)
	if legitimate.Code != http.StatusNoContent {
		t.Fatalf("same-origin break-glass status=%d body=%s", legitimate.Code, legitimate.Body.String())
	}
	reused := do(http.MethodPost, "/api/v1/auth/break-glass",
		"https://admin.example.test", "", "application/json", "", "", `{"token":"`+breakGlassToken+`"}`)
	if reused.Code != http.StatusLocked {
		t.Fatalf("emergency rate exemption bypassed rotation: %d", reused.Code)
	}
	sessionCookie := ""
	for _, entry := range legitimate.Result().Cookies() {
		if entry.Name == auth.SessionCookieName {
			sessionCookie = entry.Value
		}
	}
	if sessionCookie == "" {
		t.Fatal("same-origin break-glass did not establish a session cookie")
	}

	attack := do(http.MethodPost, "/api/v1/nodes/"+nodeID.String()+"/synthetic-commands",
		"https://sibling.example.test", "", "text/plain", sessionCookie, "csrf-poc-attack", `{"kind":"noop","expected_version":1}`)
	if attack.Code != http.StatusForbidden || !strings.Contains(attack.Body.String(), "https://ocservia.dev/problems/cross-origin-request") {
		t.Fatalf("sibling-origin mutation status=%d body=%s", attack.Code, attack.Body.String())
	}

	smuggled := do(http.MethodPost, "/api/v1/nodes/"+nodeID.String()+"/synthetic-commands",
		"https://admin.example.test", "", "text/plain", sessionCookie, "csrf-poc-smuggle", `{"kind":"noop","expected_version":1}`)
	if smuggled.Code != http.StatusUnsupportedMediaType || !strings.Contains(smuggled.Body.String(), "https://ocservia.dev/problems/unsupported-media-type") {
		t.Fatalf("text/plain mutation status=%d body=%s", smuggled.Code, smuggled.Body.String())
	}

	accepted := do(http.MethodPost, "/api/v1/nodes/"+nodeID.String()+"/synthetic-commands",
		"https://admin.example.test", "", "application/json", sessionCookie, "csrf-poc-legitimate", `{"kind":"noop","expected_version":1}`)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("legitimate mutation status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	var operationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE node_id=$1`, nodeID).Scan(&operationCount); err != nil || operationCount != 1 {
		t.Fatalf("operation count after the attack chain = %d (err=%v); only the legitimate request may create state", operationCount, err)
	}
}

func apiOperationService(pool *pgxpool.Pool) *operations.Service {
	var seed [32]byte
	seed[0] = 7
	return operations.NewBackend(postgres.WrapPool(pool), 50, commandauth.NewSignerFromSeed(seed))
}
