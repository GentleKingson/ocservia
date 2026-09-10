package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/audittest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditRBACControllerIntegration(t *testing.T) {
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
	workspace, actor, target := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, workspace, "audit", "audit-"+workspace.String(), now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{actor, target} {
		if _, err = pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, id, "audit-test", id.String(), "", "audit", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'PlatformAdmin','workspace',$4)`, uuid.New(), actor, workspace, now); err != nil {
		t.Fatal(err)
	}
	audittest.Controller(t, postgres.WrapPool(pool), workspace, actor, target, func(id, resource uuid.UUID, hash []byte, expiry time.Time) error {
		_, err := pool.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,'role_binding.elevate','role_binding',$4,'test','approved',$5,'test',$6,$7,$8,$9,$10)`, id, workspace, actor, resource, target, expiry, expiry.Add(-time.Minute), expiry.Add(-time.Hour), hash, `{}`)
		return err
	})
}
