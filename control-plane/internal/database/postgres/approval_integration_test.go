package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvaltest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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
	approvaltest.DisabledApprover(t, postgres.WrapPool(pool), workspace, requester, approver, session, approverSession, func(ctx context.Context, tx database.Tx, disabled bool) error {
		_, err := tx.Exec(ctx, `UPDATE identities SET disabled_at=CASE WHEN $2 THEN now() END WHERE id=$1`, approver, disabled)
		return err
	})
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
	approvaltest.ExpiryDuringAuthorizationWait(t, postgres.WrapPool(pool), workspace, requester, approver, session, approverSession,
		func(ctx context.Context, tx database.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM identities WHERE id=$1 FOR UPDATE`, approver).Scan(new(uuid.UUID))
		},
		func(ctx context.Context, tx database.Tx) (bool, error) {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return false, err
			}
			var blocked bool
			err := owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
			return blocked, err
		},
		func(id uuid.UUID, created, expires value.Timestamp) {
			if _, err := owner.Exec(ctx, `UPDATE approval_requests SET created_at=$2,expires_at=$3 WHERE id=$1`, id, created, expires); err != nil {
				t.Fatal(err)
			}
		})
	approvaltest.ExtendedTimes(t, postgres.WrapPool(pool), workspace, requester, approver, session, approverSession, func(id uuid.UUID, created, expires value.Timestamp) {
		if _, err := owner.Exec(ctx, `UPDATE approval_requests SET created_at=$2,expires_at=$3 WHERE id=$1`, id, created, expires); err != nil {
			t.Fatal(err)
		}
	})
	node := uuid.New()
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'approval-node','active',$3,$3)`, node, workspace, now); err != nil {
		t.Fatal(err)
	}
	approvaltest.ExpiryAfterLockWait(t, postgres.WrapPool(pool), workspace, requester, approver, session, approverSession,
		func(ctx context.Context, tx database.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, node).Scan(new(uuid.UUID))
		},
		func(ctx context.Context, tx database.Tx, id uuid.UUID) error {
			return tx.QueryRow(ctx, `SELECT id FROM approval_requests WHERE id=$1 FOR UPDATE`, id).Scan(new(uuid.UUID))
		},
		func(id uuid.UUID, created, expires value.Timestamp) {
			if _, err := owner.Exec(ctx, `UPDATE approval_requests SET created_at=$2,expires_at=$3 WHERE id=$1`, id, created, expires); err != nil {
				t.Fatal(err)
			}
		})
}
