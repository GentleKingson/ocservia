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
	if err := GrantRuntimePrivileges(ctx, pool, "ocservia_app"); err != nil {
		t.Fatal(err)
	}
	var safe bool
	if err := pool.QueryRow(ctx, `SELECT NOT has_table_privilege('ocservia_app','telemetry_rollups_5m','DELETE,TRUNCATE') AND NOT has_table_privilege('ocservia_app','telemetry_rollups_1h','DELETE,TRUNCATE') AND NOT has_function_privilege('ocservia_app','telemetry_ensure_month_partition(timestamptz)','EXECUTE') AND has_function_privilege('ocservia_app','telemetry_prune_rollups(timestamptz)','EXECUTE')`).Scan(&safe); err != nil || !safe {
		t.Fatal("current runtime privilege boundary changed with receipt numbers", safe, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_revisions SET epoch=3`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("unsupported epoch was accepted")
	}
	if _, err := pool.Exec(ctx, "UPDATE schema_revisions SET epoch=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_revisions SET checksum=decode(repeat('00',32),'hex')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("known SQL checksum corruption not rejected: %v", err)
	}
}
