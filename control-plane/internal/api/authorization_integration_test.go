package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	approvalstore "github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	configplanstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type certificateArtifactFixture struct{ data []byte }

func (f certificateArtifactFixture) FetchArtifact(context.Context, uuid.UUID, uuid.UUID, int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

type invalidatingArtifactFixture struct {
	pool       *pgxpool.Pool
	artifactID uuid.UUID
	data       []byte
}

func (f invalidatingArtifactFixture) FetchArtifact(ctx context.Context, _ uuid.UUID, _ uuid.UUID, _ int64) (io.ReadCloser, error) {
	if _, err := f.pool.Exec(ctx, `UPDATE artifact_operations SET state='failed',lease_until=NULL WHERE id=$1`, f.artifactID); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
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
	workspaceID, managerID, securityID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeA, nodeB, operationID, certificateID, artifactID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	managerBinding, securityBinding := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'certificate auth',$2,now(),now());
		INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($3,'integration',$4,now(),now()),($5,'integration',$6,now(),now());
		INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($7,$1,'cert-node-a','active',1,now(),now()),($8,$1,'cert-node-b','active',1,now(),now());
		INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,idempotency_key,request_hash,created_at,updated_at) VALUES($9,$1,$7,'succeeded',1,'certificate-auth','certificate-auth',decode(repeat('00',32),'hex'),now(),now());
		INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at) VALUES($10,$1,$7,$9,'node-a.example.test','[]',2048,'csr_pending',now(),now());
		INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at) VALUES($11,$1,$7,$10,$9,'certificate_p12','pending',decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),now()+interval '10 minutes',now(),now());
		INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($12,$3,$1,'ConfigManager','node',$7,$3,now()),($13,$5,$1,'SecurityAdmin','node',$8,$5,now())`, workspaceID, "certificate-auth-"+workspaceID.String(), managerID, managerID.String(), securityID, securityID.String(), nodeA, nodeB, operationID, certificateID, artifactID, managerBinding, securityBinding); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM role_bindings WHERE id IN($1,$2); DELETE FROM artifact_operations WHERE id=$3; DELETE FROM certificates WHERE id=$4; DELETE FROM operations WHERE id=$5; DELETE FROM nodes WHERE workspace_id=$6; DELETE FROM identities WHERE id IN($7,$8); DELETE FROM workspaces WHERE id=$6`, managerBinding, securityBinding, artifactID, certificateID, operationID, workspaceID, managerID, securityID)
	}()
	artifactData := []byte("encrypted artifact response")
	server := &Server{rbac: rbac.New(pool), certificates: certificatestore.NewWithDependencies(pool, apiOperationService(pool), nil, nil, certificateArtifactFixture{data: artifactData})}
	manager := auth.Principal{IdentityID: managerID, Issuer: "integration"}
	security := auth.Principal{IdentityID: securityID, Issuer: "integration"}
	tests := []struct {
		name, method, path, pathKey, pathValue string
		principal                              auth.Principal
		allowed                                bool
	}{
		{"manager lists own node", http.MethodGet, "/api/v1/nodes/" + nodeA.String() + "/certificates", "node_id", nodeA.String(), manager, true},
		{"manager cannot list other node", http.MethodGet, "/api/v1/nodes/" + nodeB.String() + "/certificates", "node_id", nodeB.String(), manager, false},
		{"manager issues own node", http.MethodPost, "/api/v1/nodes/" + nodeA.String() + "/certificates", "node_id", nodeA.String(), manager, true},
		{"manager creates p12", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":p12", "certificate_action", certificateID.String() + ":p12", manager, true},
		{"manager cannot revoke", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":revoke", "certificate_action", certificateID.String() + ":revoke", manager, false},
		{"cross-node security cannot revoke", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":revoke", "certificate_action", certificateID.String() + ":revoke", security, false},
		{"manager reads artifact", http.MethodGet, "/api/v1/artifacts/" + artifactID.String(), "artifact_id", artifactID.String(), manager, true},
		{"cross-node security cannot read artifact", http.MethodGet, "/api/v1/artifacts/" + artifactID.String(), "artifact_id", artifactID.String(), security, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.SetPathValue(test.pathKey, test.pathValue)
			_, err := server.authorizeRoute(request, test.principal)
			if test.allowed && err != nil {
				t.Fatalf("expected authorization: %v", err)
			}
			if !test.allowed && !errors.Is(err, rbac.ErrForbidden) {
				t.Fatalf("expected forbidden, got %v", err)
			}
		})
	}
	token := strings.Repeat("a", 43)
	tokenHash, artifactHash := sha256.Sum256([]byte(token)), sha256.Sum256(artifactData)
	if _, err := pool.Exec(ctx, `UPDATE certificates SET state='issued',certificate_chain_pem=decode(repeat('41',64),'hex'),serial_number='1',not_before=now()-interval '1 minute',not_after=now()+interval '1 hour' WHERE id=$1; UPDATE artifact_operations SET state='ready',token_sha256=$3,content_sha256=$4,content_size=$5 WHERE id=$2`, certificateID, artifactID, tokenHash[:], artifactHash[:], len(artifactData)); err != nil {
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
	failureServer := &Server{certificates: certificatestore.NewWithDependencies(pool, apiOperationService(pool), nil, nil, invalidatingArtifactFixture{pool: pool, artifactID: artifactID, data: artifactData})}
	failureRequest := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+artifactID.String(), nil)
	failureRequest.SetPathValue("artifact_id", artifactID.String())
	failureRequest.Header.Set("X-Artifact-Token", token)
	failureRequest = failureRequest.WithContext(context.WithValue(context.WithValue(failureRequest.Context(), principalKey{}, manager), requestIDKey{}, "artifact-completion-failure"))
	failureResponse := httptest.NewRecorder()
	failureServer.downloadArtifact(failureResponse, failureRequest)
	if failureResponse.Code != http.StatusForbidden || failureResponse.Header().Get("Content-Disposition") != "" || failureResponse.Header().Get("Content-Length") != "" || !strings.Contains(failureResponse.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("completion failure status=%d headers=%v body=%s", failureResponse.Code, failureResponse.Header(), failureResponse.Body.String())
	}
}

func TestSyntheticCommandAuditUsesAuthenticatedOperator(t *testing.T) {
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
	workspaceID, identityID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeID, sessionID, bindingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'synthetic audit',$2,now(),now());
		INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($3,'integration',$4,now(),now());
		INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($5,$3,now()+interval '1 hour',now());
		INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($6,$1,'audit-node','active',1,now(),now());
		INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($7,$3,$1,'Operator','node',$6,$3,now())`,
		workspaceID, "synthetic-audit-"+workspaceID.String(), identityID, identityID.String(), sessionID, nodeID, bindingID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, statement := range []string{
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM role_bindings WHERE id=$2`,
			`DELETE FROM nodes WHERE workspace_id=$1`, `DELETE FROM auth_sessions WHERE id=$3`,
			`DELETE FROM identities WHERE id=$4`, `DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(context.Background(), statement, workspaceID, bindingID, sessionID, identityID)
		}
	}()

	server := &Server{rbac: rbac.New(pool), operations: apiOperationService(pool)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+nodeID.String()+"/synthetic-commands", strings.NewReader(`{"kind":"noop","expected_version":1}`))
	request.SetPathValue("node_id", nodeID.String())
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

func TestConfigPlanApprovalResolvesNodeScopedApprover(t *testing.T) {
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
	workspaceID, nodeID, operationID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	requesterID, approverID, approvalID, bindingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'config approval',$2,now(),now());
		INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($3,'integration',$4,now(),now()),($5,'integration',$6,now(),now());
		INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($7,$1,'config-node','active',1,now(),now());
		INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,idempotency_key,request_hash,created_at,updated_at) VALUES($8,$1,$7,'succeeded',1,'config-approval','config-approval',decode(repeat('00',32),'hex'),now(),now());
		INSERT INTO config_plans(id,workspace_id,node_id,operation_id,template_name,expected_revision,candidate_hash,candidate_redacted,warnings,expires_at,created_by,created_at) VALUES($8,$1,$7,$8,'approval',0,decode(repeat('01',32),'hex'),'tcp-port = 443','[]',now()+interval '1 hour',$3,now());
		INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary) VALUES($9,$1,$3,'config.apply','config_plan',$8,'review','pending',now()+interval '1 hour',now(),decode(repeat('01',32),'hex'),'{}');
		INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($10,$5,$1,'SecurityAdmin','node',$7,$5,now())`, workspaceID, "config-approval-"+workspaceID.String(), requesterID, requesterID.String(), approverID, approverID.String(), nodeID, operationID, approvalID, bindingID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM role_bindings WHERE id=$1; DELETE FROM approval_requests WHERE id=$2; DELETE FROM config_plans WHERE id=$3; DELETE FROM operations WHERE id=$3; DELETE FROM nodes WHERE id=$4; DELETE FROM identities WHERE id IN($5,$6); DELETE FROM workspaces WHERE id=$7`, bindingID, approvalID, operationID, nodeID, requesterID, approverID, workspaceID)
	}()
	operationService := apiOperationService(pool)
	server := &Server{rbac: rbac.New(pool), approvals: approvalstore.New(pool), configplans: configplanstore.New(pool, operationService)}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/approval-requests/"+approvalID.String(), nil)
	request.SetPathValue("approval_id", approvalID.String())
	if _, err := server.authorizeRoute(request, auth.Principal{IdentityID: approverID, Issuer: "integration"}); err != nil {
		t.Fatalf("node-scoped config approver: %v", err)
	}
}

func apiOperationService(pool *pgxpool.Pool) *operationstore.Service {
	var seed [32]byte
	seed[0] = 7
	return operationstore.NewWithSigner(pool, 50, commandauth.NewSignerFromSeed(seed))
}

func TestBatchRouteAllowsNodeScopedPerItemAuthorization(t *testing.T) {
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
	workspaceID, identityID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeA, nodeB, bindingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'batch auth',$2,now(),now());
		INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($3,'integration',$4,now(),now());
		INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($5,$1,'node-a','active',1,now(),now()),($6,$1,'node-b','active',1,now(),now());
		INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($7,$3,$1,'UserManager','node',$5,$3,now())`, workspaceID, "batch-auth-"+workspaceID.String(), identityID, identityID.String(), nodeA, nodeB, bindingID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM role_bindings WHERE id=$1; DELETE FROM nodes WHERE workspace_id=$2; DELETE FROM identities WHERE id=$3; DELETE FROM workspaces WHERE id=$2`, bindingID, workspaceID, identityID)
	}()

	server := &Server{rbac: rbac.New(pool)}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test/api/v1/user-batches", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Workspace-ID", workspaceID.String())
	authorized, err := server.authorizeRoute(request, auth.Principal{IdentityID: identityID, Issuer: "integration"})
	if err != nil {
		t.Fatalf("node-scoped batch route: %v", err)
	}
	request = request.WithContext(authorized)
	if workspace(request) != workspaceID {
		t.Fatalf("node-scoped batch route workspace=%s", workspace(request))
	}
	resourceA, _ := server.rbac.Node(ctx, nodeA)
	resourceB, _ := server.rbac.Node(ctx, nodeB)
	if err := server.rbac.Authorize(ctx, identityID, "user.manage", resourceA, false); err != nil {
		t.Fatalf("authorized item A: %v", err)
	}
	if err := server.rbac.Authorize(ctx, identityID, "user.manage", resourceB, false); err == nil {
		t.Fatal("unauthorized item B unexpectedly passed")
	}
}
