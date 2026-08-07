package approvals

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTwoPersonApprovalAndSingleConsumptionIntegration(t *testing.T) {
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
	workspaceID, nodeID, requesterID, approverID, requesterSession, approverSession := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'I12 approvals',$2,now(),now())`, workspaceID, "i12-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'node','active',now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, requesterID, "requester-"+requesterID.String(), approverID, "approver-"+approverID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES($1,$2,now()+interval '1 hour',now()),($3,$4,now()+interval '1 hour',now())`, requesterSession, requesterID, approverSession, approverID); err != nil {
		t.Fatal(err)
	}
	service := New(pool)
	approval, err := service.Create(ctx, Request{WorkspaceID: workspaceID, RequesterID: requesterID, ResourceID: nodeID, Action: "service.reload", ResourceType: "node", Reason: "planned maintenance", TTL: time.Hour, SessionID: requesterSession, RequestID: "request-create"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Approve(ctx, Decision{ApprovalID: approval.ID, ApproverID: requesterID, SessionID: requesterSession, Reason: "self", RequestID: "request-self"}); !errors.Is(err, ErrSelf) {
		t.Fatalf("self approval error = %v", err)
	}
	approved, err := service.Approve(ctx, Decision{ApprovalID: approval.ID, ApproverID: approverID, SessionID: approverSession, Reason: "independent review", RequestID: "request-approve"})
	if err != nil || approved.Status != "approved" {
		t.Fatalf("approval = %+v, %v", approved, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Consume(ctx, tx, approval.ID, workspaceID, requesterID, "service.reload", "node", nodeID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := Consume(ctx, tx, approval.ID, workspaceID, requesterID, "service.reload", "node", nodeID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("reused approval error = %v", err)
	}
	boundHash := make([]byte, 32)
	boundHash[0] = 0x42
	bound, err := service.Create(ctx, Request{WorkspaceID: workspaceID, RequesterID: requesterID, ResourceID: uuid.Must(uuid.NewV7()), Action: "user.batch.disable", ResourceType: "batch_operation", Reason: "bulk review", TTL: time.Hour, SessionID: requesterSession, RequestID: "request-bound", RequestHash: boundHash, RequestSummary: []byte(`[{"node_id":"019fc0a4-6d92-765c-a8a1-4af556614cc3","username":"alice","action":"disable","expected_version":1}]`)})
	if err != nil {
		t.Fatal(err)
	}
	details, err := service.Get(ctx, bound.ID)
	if err != nil || details.RequestHash == "" || len(details.RequestSummary) == 0 {
		t.Fatalf("bound approval details=%+v err=%v", details, err)
	}
	if _, err := service.Approve(ctx, Decision{ApprovalID: bound.ID, ApproverID: approverID, SessionID: approverSession, Reason: "wrong digest", RequestID: "request-bound-wrong", ExpectedRequestHash: "wrong"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("wrong bound hash approval err=%v", err)
	}
	if _, err := service.Approve(ctx, Decision{ApprovalID: bound.ID, ApproverID: approverID, SessionID: approverSession, Reason: "reviewed details", RequestID: "request-bound-approve", ExpectedRequestHash: details.RequestHash}); err != nil {
		t.Fatalf("approve reviewed bound content: %v", err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND approval_id=$2`, workspaceID, approval.ID).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("approval audit count = %d, %v", auditCount, err)
	}
}
