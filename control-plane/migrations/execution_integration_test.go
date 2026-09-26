package migrations

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrationFailureIsAtomicIntegration(t *testing.T) {
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

	beforeVersion, err := readCurrentSchemaVersion(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failed := applyMigration(ctx, conn, Migration{
		Version:  beforeVersion + 1,
		Name:     "000030_failed_test.up.sql",
		SQL:      "SELECT 1 / 0",
		Checksum: sha256.Sum256([]byte("SELECT 1 / 0")),
	}, nil)
	conn.Release()
	if failed == nil {
		t.Fatal("failed migration unexpectedly succeeded")
	}

	afterVersion, err := readCurrentSchemaVersion(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if afterVersion != beforeVersion {
		t.Fatalf("schema version changed after failed migration: before=%d after=%d", beforeVersion, afterVersion)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE version = $1", beforeVersion+1).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration was recorded: count=%d", count)
	}
}
