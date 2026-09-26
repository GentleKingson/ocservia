package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The script supplies a separate empty database for current initialization.
func TestDatabaseInitializationSmoke(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_INITIALIZATION_DATABASE_URL")
	if url == "" {
		t.Skip("isolated initialization database required")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'initialization smoke','initialization-smoke',now(),now())`, id); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, id).Scan(&name); err != nil || name != "initialization smoke" {
		t.Fatal(name, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES(9000001,'unknown-receipt',decode(repeat('01',32),'hex'))`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("unknown receipt became a version gate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET checksum=decode(repeat('00',32),'hex') WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "checksum does not match") {
		t.Fatalf("known SQL checksum corruption not rejected: %v", err)
	}
}
