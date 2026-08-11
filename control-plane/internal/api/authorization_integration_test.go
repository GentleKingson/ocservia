package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
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

func (f certificateArtifactFixture) FetchArtifact(context.Context, *agentv1.ArtifactGrantV1) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (certificateArtifactFixture) ConsumeArtifact(context.Context, *agentv1.ArtifactGrantV1, []byte, int64) error {
	return nil
}

func (certificateArtifactFixture) ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64) (bool, error) {
	return true, nil
}

type invalidatingArtifactFixture struct {
	pool       *pgxpool.Pool
	artifactID uuid.UUID
	data       []byte
}

func (f invalidatingArtifactFixture) FetchArtifact(ctx context.Context, _ *agentv1.ArtifactGrantV1) (io.ReadCloser, error) {
	if _, err := f.pool.Exec(ctx, `UPDATE artifact_operations SET state='failed',lease_until=NULL WHERE id=$1`, f.artifactID); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (invalidatingArtifactFixture) ConsumeArtifact(context.Context, *agentv1.ArtifactGrantV1, []byte, int64) error {
	return nil
}

func (invalidatingArtifactFixture) ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64) (bool, error) {
	return true, nil
}

type terminalArtifactFixture struct {
	pool       *pgxpool.Pool
	artifactID uuid.UUID
	state      string
	data       []byte
}

func (f terminalArtifactFixture) FetchArtifact(context.Context, *agentv1.ArtifactGrantV1) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (f terminalArtifactFixture) ConsumeArtifact(ctx context.Context, _ *agentv1.ArtifactGrantV1, _ []byte, _ int64) error {
	command, err := f.pool.Exec(ctx, `UPDATE artifact_operations SET state=$2,lease_until=NULL,active_grant_expires_at=now(),updated_at=now() WHERE id=$1 AND state='consuming'`, f.artifactID, f.state)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return errors.New("terminal artifact race did not win")
	}
	return nil
}

func (terminalArtifactFixture) ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64) (bool, error) {
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
	nodeA, nodeB, operationID, certificateID, artifactID, approvalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	managerBinding, securityBinding := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'certificate auth',$2,now(),now());
		INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($3,'integration',$4,now(),now()),($5,'integration',$6,now(),now());
		INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($7,$1,'cert-node-a','active',1,now(),now()),($8,$1,'cert-node-b','active',1,now(),now());
		INSERT INTO operations(id,workspace_id,node_id,state,version,request_id,idempotency_key,request_hash,created_at,updated_at) VALUES($9,$1,$7,'succeeded',1,'certificate-auth','certificate-auth',decode(repeat('00',32),'hex'),now(),now());
		INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at) VALUES($10,$1,$7,$9,'node-a.example.test','[]',2048,'csr_pending',now(),now());
		INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,consumed_at,created_at,authority_snapshot_at) VALUES($14,$1,$3,'certificate.private_key.export','certificate',$10,'fixture export','consumed',$5,'independent fixture review',now()+interval '10 minutes',now(),now(),now()-interval '1 minute',now()-interval '1 minute');
		INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,certificate_version,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at,approval_id) VALUES($11,$1,$7,$10,1,$9,'certificate_p12','pending',decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),now()+interval '10 minutes',now(),now(),$14);
		INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($12,$3,$1,'ConfigManager','node',$7,$3,now()),($13,$5,$1,'SecurityAdmin','node',$8,$5,now())`, workspaceID, "certificate-auth-"+workspaceID.String(), managerID, managerID.String(), securityID, securityID.String(), nodeA, nodeB, operationID, certificateID, artifactID, managerBinding, securityBinding, approvalID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM role_bindings WHERE id IN($1,$2); DELETE FROM artifact_operations WHERE id=$3; DELETE FROM approval_requests WHERE id=$9; DELETE FROM certificates WHERE id=$4; DELETE FROM operations WHERE id=$5; DELETE FROM nodes WHERE workspace_id=$6; DELETE FROM identities WHERE id IN($7,$8); DELETE FROM workspaces WHERE id=$6`, managerBinding, securityBinding, artifactID, certificateID, operationID, workspaceID, managerID, securityID, approvalID)
	}()
	artifactData := []byte("encrypted artifact response")
	grantSigner := commandauth.NewSignerFromSeed([32]byte{7})
	server := &Server{rbac: rbac.New(pool), certificates: certificatestore.NewWithDependencies(pool, apiOperationService(pool), nil, nil, certificateArtifactFixture{data: artifactData}, grantSigner)}
	manager := auth.Principal{IdentityID: managerID, SessionID: uuid.Must(uuid.NewV7()), Issuer: "integration"}
	security := auth.Principal{IdentityID: securityID, SessionID: uuid.Must(uuid.NewV7()), Issuer: "integration"}
	tests := []struct {
		name, method, path, pathKey, pathValue string
		principal                              auth.Principal
		allowed                                bool
	}{
		{"manager lists own node", http.MethodGet, "/api/v1/nodes/" + nodeA.String() + "/certificates", "node_id", nodeA.String(), manager, true},
		{"manager cannot list other node", http.MethodGet, "/api/v1/nodes/" + nodeB.String() + "/certificates", "node_id", nodeB.String(), manager, false},
		{"manager issues own node", http.MethodPost, "/api/v1/nodes/" + nodeA.String() + "/certificates", "node_id", nodeA.String(), manager, true},
		{"manager cannot create p12", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":p12", "certificate_action", certificateID.String() + ":p12", manager, false},
		{"manager cannot revoke", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":revoke", "certificate_action", certificateID.String() + ":revoke", manager, false},
		{"cross-node security cannot revoke", http.MethodPost, "/api/v1/certificates/" + certificateID.String() + ":revoke", "certificate_action", certificateID.String() + ":revoke", security, false},
		{"manager cannot use read permission to export artifact", http.MethodGet, "/api/v1/artifacts/" + artifactID.String(), "artifact_id", artifactID.String(), manager, false},
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
	failureServer := &Server{certificates: certificatestore.NewWithDependencies(pool, apiOperationService(pool), nil, nil, invalidatingArtifactFixture{pool: pool, artifactID: artifactID, data: artifactData}, grantSigner)}
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
			terminalServer := &Server{certificates: certificatestore.NewWithDependencies(pool, apiOperationService(pool), nil, nil, terminalArtifactFixture{pool: pool, artifactID: artifactID, state: terminalState, data: artifactData}, grantSigner)}
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

func TestApprovalDetailRequiresEveryAuthorityScopeIntegration(t *testing.T) {
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

	workspaceID, approvalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeA, nodeB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if nodeA.String() > nodeB.String() {
		nodeA, nodeB = nodeB, nodeA
	}
	requesterID := uuid.Must(uuid.NewV7())
	partialID := uuid.Must(uuid.NewV7())
	completeID := uuid.Must(uuid.NewV7())
	workspaceSecurityID := uuid.Must(uuid.NewV7())
	workspacePlatformID := uuid.Must(uuid.NewV7())
	identityIDs := []uuid.UUID{requesterID, partialID, completeID, workspaceSecurityID, workspacePlatformID}
	bindingIDs := []uuid.UUID{
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
	}
	summary, err := json.Marshal([]map[string]any{
		{"node_id": nodeA, "username": "node-a-user", "action": "disable", "expected_version": 1},
		{"node_id": nodeB, "username": "node-b-user", "action": "disable", "expected_version": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256(summary)

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'approval scope auth',$2,now(),now())`, []any{workspaceID, "approval-scope-auth-" + workspaceID.String()}},
		{`INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES
			($1,'integration',$2,now(),now()),($3,'integration',$4,now(),now()),($5,'integration',$6,now(),now()),
			($7,'integration',$8,now(),now()),($9,'integration',$10,now(),now())`, []any{requesterID, requesterID.String(), partialID, partialID.String(), completeID, completeID.String(), workspaceSecurityID, workspaceSecurityID.String(), workspacePlatformID, workspacePlatformID.String()}},
		{`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$3,'approval-node-a','active',1,now(),now()),($2,$3,'approval-node-b','active',1,now(),now())`, []any{nodeA, nodeB, workspaceID}},
		{`INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary,authority_snapshot_at)
			VALUES($1,$2,$3,'user.batch.disable','batch_operation',$1,'review exact batch','pending',now()+interval '1 hour',now(),$4,$5,now())`, []any{approvalID, workspaceID, requesterID, requestHash[:], summary}},
		{`INSERT INTO approval_authority_resources(approval_id,workspace_id,resource_type,resource_id) VALUES($1,$2,'node',$3),($1,$2,'node',$4)`, []any{approvalID, workspaceID, nodeA, nodeB}},
		{`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES
			($1,$6,$10,'SecurityAdmin','node',$11,NULL,now()),
			($2,$7,$10,'SecurityAdmin','node',$11,NULL,now()),($3,$7,$10,'SecurityAdmin','node',$12,NULL,now()),
			($4,$8,$10,'SecurityAdmin','workspace',NULL,NULL,now()),($5,$9,$10,'PlatformAdmin','workspace',NULL,NULL,now())`, []any{bindingIDs[0], bindingIDs[1], bindingIDs[2], bindingIDs[3], bindingIDs[4], partialID, completeID, workspaceSecurityID, workspacePlatformID, workspaceID, nodeA, nodeB}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM role_bindings WHERE id=ANY($1)`, []any{bindingIDs}},
			{`DELETE FROM approval_requests WHERE id=$1`, []any{approvalID}},
			{`DELETE FROM nodes WHERE workspace_id=$1`, []any{workspaceID}},
			{`DELETE FROM identities WHERE id=ANY($1)`, []any{identityIDs}},
			{`DELETE FROM workspaces WHERE id=$1`, []any{workspaceID}},
		} {
			_, _ = pool.Exec(context.Background(), statement.query, statement.args...)
		}
	}()

	server := &Server{rbac: rbac.New(pool), approvals: approvalstore.New(pool)}
	readApproval := func(identityID uuid.UUID) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/approval-requests/"+approvalID.String(), nil)
		request.SetPathValue("approval_id", approvalID.String())
		response := httptest.NewRecorder()
		authorized, err := server.authorizeRoute(request, auth.Principal{IdentityID: identityID, Issuer: "integration"})
		if err != nil {
			server.writeAuthorizationError(response, request, err)
			return response
		}
		server.getApproval(response, request.WithContext(authorized))
		return response
	}

	partial := readApproval(partialID)
	if partial.Code != http.StatusForbidden || strings.Contains(partial.Body.String(), "node-b-user") {
		t.Fatalf("partial reviewer status=%d body=%s", partial.Code, partial.Body.String())
	}
	for name, identityID := range map[string]uuid.UUID{
		"all node scopes":         completeID,
		"workspace SecurityAdmin": workspaceSecurityID,
		"workspace PlatformAdmin": workspacePlatformID,
	} {
		t.Run(name, func(t *testing.T) {
			response := readApproval(identityID)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "node-a-user") || !strings.Contains(response.Body.String(), "node-b-user") {
				t.Fatalf("authorized reviewer status=%d body=%s", response.Code, response.Body.String())
			}
		})
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
