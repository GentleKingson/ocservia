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

func TestAuditControllerIntegration(t *testing.T) {
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
	workspace, actor := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, workspace, "audit", "audit-"+workspace.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, actor, "audit-test", actor.String(), "", "audit", now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,$3)`, workspace, actor, now); err != nil {
		t.Fatal(err)
	}
	audittest.Controller(t, postgres.WrapPool(pool), workspace, actor)
}
