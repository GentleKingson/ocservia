package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// The script supplies a separate empty database for schema 35 -> current.
func TestDatabaseUpgradeSmoke(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_UPGRADE_DATABASE_URL")
	if url == "" {
		t.Skip("isolated upgrade database required")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (version bigint PRIMARY KEY, name text NOT NULL, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	steps, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.Version > 35 {
			break
		}
		if err := applyMigration(ctx, conn, step, nil); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'upgrade smoke','upgrade-smoke',now(),now())`, id); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, id).Scan(&name); err != nil || name != "upgrade smoke" {
		t.Fatal(name, err)
	}
}
