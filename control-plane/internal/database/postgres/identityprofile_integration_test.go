package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIdentityProfilesIntegration(t *testing.T) {
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
	b := WrapPool(pool)
	semantictest.IdentityProfiles(t, b, func(id uuid.UUID) error {
		_, err := b.Exec(ctx, `UPDATE identities SET disabled_at=now() WHERE id=$1`, id)
		return err
	})
}
