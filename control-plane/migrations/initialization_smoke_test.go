//go:build integration

package migrations

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	if err := ValidateSchemaVersion(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'initialization smoke','initialization-smoke',now(),now())`, id); err != nil {
		t.Fatal(err)
	}
	// Re-running Migrate must be idempotent and preserve data.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, id).Scan(&name); err != nil || name != "initialization smoke" {
		t.Fatal(name, err)
	}
	var backlogIndexes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname IN ('commands_queued_expiry_idx','operations_node_backlog_idx','operations_workspace_backlog_idx')`).Scan(&backlogIndexes); err != nil || backlogIndexes != 3 {
		t.Fatalf("missing backlog/expiry partial indexes: count=%d err=%v", backlogIndexes, err)
	}
	if err := GrantRuntimePrivileges(ctx, pool, "ocservia_app"); err != nil {
		t.Fatal(err)
	}
	var safe bool
	if err := pool.QueryRow(ctx, `SELECT NOT has_table_privilege('ocservia_app','telemetry_rollups_5m','DELETE,TRUNCATE') AND NOT has_table_privilege('ocservia_app','telemetry_rollups_1h','DELETE,TRUNCATE') AND NOT has_function_privilege('ocservia_app','telemetry_ensure_month_partition(timestamptz)','EXECUTE') AND has_function_privilege('ocservia_app','telemetry_ensure_upcoming_partitions()','EXECUTE') AND has_function_privilege('ocservia_app','telemetry_prune_rollups(timestamptz)','EXECUTE') AND has_table_privilege('ocservia_app','goose_db_version','SELECT')`).Scan(&safe); err != nil || !safe {
		t.Fatal("runtime privilege boundary mismatch", safe, err)
	}
	t.Run("resume retired telemetry partition", func(t *testing.T) {
		month := time.Now().UTC().Truncate(time.Second)
		month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -2, 0)
		name := "telemetry_samples_" + month.Format("200601")
		ident := pgx.Identifier{"public", name}.Sanitize()
		partitionSQL := fmt.Sprintf("CREATE TABLE %s PARTITION OF public.telemetry_samples FOR VALUES FROM ('%s') TO ('%s')", ident, month.Format(time.RFC3339), month.AddDate(0, 1, 0).Format(time.RFC3339))
		if _, err := pool.Exec(ctx, partitionSQL); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO public.telemetry_partition_retirements(partition_name,relation_oid) VALUES($1,to_regclass($2))`, name, ident); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "ALTER TABLE public.telemetry_samples DETACH PARTITION "+ident+" CONCURRENTLY"); err != nil {
			t.Fatal(err)
		}
		if err := PruneTelemetryPartitions(ctx, pool); err != nil {
			t.Fatal(err)
		}
		var clean bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NULL AND NOT EXISTS (SELECT 1 FROM public.telemetry_partition_retirements)`, ident).Scan(&clean); err != nil || !clean {
			t.Fatal("retirement failed to clean up detached table and marker", clean, err)
		}
		if err := Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	})
}
