package testfixture

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenRuntime connects to the shared integration database configured in
// OCSERV_TEST_DATABASE_URL, or skips when running without PostgreSQL.
func OpenRuntime(t testing.TB) *postgres.Backend {
	t.Helper()
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL required")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.WrapPool(pool)
}

// OpenIsolated creates a temporary migrated PostgreSQL database owned by the
// test owner account and returns pools for both the runtime and owner roles.
// If cleanupT differs from t, cleanup is bound to cleanupT so parent fixtures
// survive until subtests complete.
func OpenIsolated(t, cleanupT testing.TB, prefix string) (runtime, owner *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	runtimeURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if runtimeURL == "" {
		t.Skip("real PostgreSQL required")
	}
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerURL == "" {
		t.Fatal("OCSERV_TEST_OWNER_DATABASE_URL required for isolated PostgreSQL database")
	}
	admin, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	cleanupT.Cleanup(admin.Close)
	name := prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	cleanupT.Cleanup(func() {
		// Reset idle admin connections so fixtures that restart PostgreSQL can
		// still drop their temporary database reliably.
		admin.Reset()
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			cleanupT.Error("drop isolated fixture database", err)
		}
	})
	open := func(raw string) *pgxpool.Pool {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Database = name
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		cleanupT.Cleanup(pool.Close)
		return pool
	}
	owner = open(ownerURL)
	runtime = open(runtimeURL)
	if err := migrations.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := migrations.GrantRuntimePrivileges(ctx, owner, runtime.Config().ConnConfig.User); err != nil {
		t.Fatal(err)
	}
	return runtime, owner
}
