package rbac

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestObjectAndFunctionAuthorizationIntegration(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	identityID, workspaceOne, workspaceTwo, nodeOne, nodeTwo, bindingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'one',$2,now(),now()),($3,'two',$4,now(),now())`, workspaceOne, "one-"+workspaceOne.String(), workspaceTwo, "two-"+workspaceTwo.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'one','active',now(),now()),($3,$4,'two','active',now(),now())`, nodeOne, workspaceOne, nodeTwo, workspaceTwo); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'test',$2,now(),now())`, identityID, "viewer-"+identityID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at)VALUES($1,$2,$3,'Viewer','workspace',now())`, bindingID, identityID, workspaceOne); err != nil {
		t.Fatal(err)
	}
	service := New(pool)
	if err := service.Authorize(ctx, identityID, "node.read", Resource{WorkspaceID: workspaceOne, Type: "node", ID: nodeOne}, false); err != nil {
		t.Fatal(err)
	}
	if err := service.Authorize(ctx, identityID, "node.read", Resource{WorkspaceID: workspaceTwo, Type: "node", ID: nodeTwo}, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("BOLA check error = %v", err)
	}
	if err := service.Authorize(ctx, identityID, "node.revoke", Resource{WorkspaceID: workspaceOne, Type: "node", ID: nodeOne}, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("BFLA check error = %v", err)
	}
}
