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

func TestRoleGrantCannotExceedActorPermissionsIntegration(t *testing.T) {
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

	workspaceID := uuid.Must(uuid.NewV7())
	securityID, platformID, targetID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	securitySession, platformSession := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'grant ceiling',$2,now(),now())`, workspaceID, "grant-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now()),($5,'test',$6,now(),now())`, securityID, "security-"+securityID.String(), platformID, "platform-"+platformID.String(), targetID, "target-"+targetID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now()),($3,$4,now()+interval '1 hour',now())`, securitySession, securityID, platformSession, platformID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'SecurityAdmin','workspace',now()),($4,$5,$3,'PlatformAdmin','workspace',now())`, uuid.Must(uuid.NewV7()), securityID, workspaceID, uuid.Must(uuid.NewV7()), platformID); err != nil {
		t.Fatal(err)
	}

	service := New(pool)
	base := BindingRequest{IdentityID: securityID, WorkspaceID: workspaceID, ActorID: securityID, SessionID: securitySession, ResourceType: "workspace", RequestID: uuid.Must(uuid.NewV7()).String(), Reason: "grant ceiling test"}
	base.Role = "PlatformAdmin"
	if _, err := service.CreateBinding(ctx, base); !errors.Is(err, ErrGrantForbidden) {
		t.Fatalf("SecurityAdmin self-promotion error = %v", err)
	}
	base.IdentityID = targetID
	base.Role = "Operator"
	base.RequestID = uuid.Must(uuid.NewV7()).String()
	if _, err := service.CreateBinding(ctx, base); !errors.Is(err, ErrGrantForbidden) {
		t.Fatalf("SecurityAdmin Operator grant error = %v", err)
	}
	base.Role = "Viewer"
	base.RequestID = uuid.Must(uuid.NewV7()).String()
	if _, err := service.CreateBinding(ctx, base); err != nil {
		t.Fatalf("SecurityAdmin Viewer grant error = %v", err)
	}
	base.ActorID, base.SessionID = platformID, platformSession
	base.Role = "PlatformAdmin"
	base.RequestID = uuid.Must(uuid.NewV7()).String()
	if _, err := service.CreateBinding(ctx, base); err != nil {
		t.Fatalf("PlatformAdmin PlatformAdmin grant error = %v", err)
	}
}
