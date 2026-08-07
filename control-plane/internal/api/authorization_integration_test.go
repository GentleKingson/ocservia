package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

	server := &Server{rbac: rbac.New(pool), operations: operationstore.New(pool)}
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
