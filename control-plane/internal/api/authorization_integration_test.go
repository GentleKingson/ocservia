package api

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
