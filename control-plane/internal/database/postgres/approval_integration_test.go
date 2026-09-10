package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvaltest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApprovalControllerWorkflowIntegration(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspace, requester, approver, session, approverSession := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, workspace, "approval", "approval-"+workspace.String(), now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{requester, approver} {
		if _, err = pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, id, "approval-test", id.String(), "", "approval", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]uuid.UUID{{session, requester}, {approverSession, approver}} {
		if _, err = pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,$3,$4)`, pair[0], pair[1], now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'SecurityAdmin','workspace',$4)`, uuid.New(), approver, workspace, now); err != nil {
		t.Fatal(err)
	}
	approvaltest.Workflow(t, postgres.WrapPool(pool), workspace, requester, approver, session, approverSession)
	ownerDSN := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerDSN == "" {
		ownerDSN = dsn
	}
	ownerPool, err := pgxpool.New(ctx, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	owner := postgres.WrapPool(ownerPool)
	approvaltest.ExtendedTimes(t, postgres.WrapPool(pool), workspace, requester, approver, session, approverSession, func(id uuid.UUID, created, expires value.Timestamp) {
		if _, err := owner.Exec(ctx, `UPDATE approval_requests SET created_at=$2,expires_at=$3 WHERE id=$1`, id, created, expires); err != nil {
			t.Fatal(err)
		}
	})
}
