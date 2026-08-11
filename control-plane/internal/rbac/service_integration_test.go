package rbac

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
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
	securityID, platformID, targetID, reviewerID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	securitySession, platformSession, reviewerSession := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'grant ceiling',$2,now(),now())`, workspaceID, "grant-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now()),($5,'test',$6,now(),now()),($7,'test',$8,now(),now())`, securityID, "security-"+securityID.String(), platformID, "platform-"+platformID.String(), targetID, "target-"+targetID.String(), reviewerID, "reviewer-"+reviewerID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now()),($3,$4,now()+interval '1 hour',now()),($5,$6,now()+interval '1 hour',now())`, securitySession, securityID, platformSession, platformID, reviewerSession, reviewerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'SecurityAdmin','workspace',now()-interval '1 minute'),($4,$5,$3,'PlatformAdmin','workspace',now()-interval '1 minute'),($6,$7,$3,'PlatformAdmin','workspace',now()-interval '1 minute')`, uuid.Must(uuid.NewV7()), securityID, workspaceID, uuid.Must(uuid.NewV7()), platformID, uuid.Must(uuid.NewV7()), reviewerID); err != nil {
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
	if _, err := service.CreateBinding(ctx, base); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("unapproved PlatformAdmin grant error = %v", err)
	}
	hash, summary := BindingApprovalContent(targetID, workspaceID, "PlatformAdmin", "workspace", uuid.Nil)
	approvalService := approvals.New(pool)
	approval, err := approvalService.Create(ctx, approvals.Request{WorkspaceID: workspaceID, RequesterID: platformID, ResourceID: targetID, Action: "role_binding.elevate", ResourceType: "role_binding", Reason: "independent role elevation", TTL: time.Hour, SessionID: platformSession, RequestID: uuid.Must(uuid.NewV7()).String(), RequestHash: hash, RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspaceID, Type: "workspace", ID: uuid.Nil}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approvalService.Approve(ctx, approvals.Decision{ApprovalID: approval.ID, ApproverID: targetID, SessionID: reviewerSession, Reason: "circular approval", RequestID: uuid.Must(uuid.NewV7()).String(), ExpectedRequestHash: approval.RequestHash}); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("new target supplied circular approval: %v", err)
	}
	if _, err := approvalService.Approve(ctx, approvals.Decision{ApprovalID: approval.ID, ApproverID: reviewerID, SessionID: reviewerSession, Reason: "independent review", RequestID: uuid.Must(uuid.NewV7()).String(), ExpectedRequestHash: approval.RequestHash}); err != nil {
		t.Fatal(err)
	}
	base.ApprovalID = approval.ID
	base.RequestID = uuid.Must(uuid.NewV7()).String()
	if _, err := service.CreateBinding(ctx, base); err != nil {
		t.Fatalf("approved PlatformAdmin grant error = %v", err)
	}
}

func TestResourceTypePreventsUUIDScopeAliasIntegration(t *testing.T) {
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
	workspaceID, identityID, sharedID, bindingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'typed scope',$2,now(),now())`, workspaceID, "typed-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'test',$2,now(),now())`, identityID, "typed-"+identityID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at)VALUES($1,$2,$3,'ConfigManager','secret_ref',$4,now())`, bindingID, identityID, workspaceID, sharedID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM role_bindings WHERE id=$1`, bindingID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM identities WHERE id=$1`, identityID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, workspaceID)
	}()
	service := New(pool)
	if err := service.Authorize(ctx, identityID, "secret.use", Resource{WorkspaceID: workspaceID, Type: "secret_ref", ID: sharedID}, false); err != nil {
		t.Fatal(err)
	}
	if err := service.Authorize(ctx, identityID, "secret.use", Resource{WorkspaceID: workspaceID, Type: "resource", ID: sharedID}, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-type UUID alias accepted: %v", err)
	}
}
