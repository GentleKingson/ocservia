package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This fixture is read from immutable Git history, never an archive copy in the
// runtime tree. Both the released tag and the exact release SHA are mandatory.
func releasedCheckpoint(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sha := "169102557cd610847c9f6ac2083336cdcf82c483"
	tag, err := exec.Command("git", "rev-parse", "v1.2.0^{commit}").Output()
	if err != nil || strings.TrimSpace(string(tag)) != sha {
		t.Fatal("missing or moved designated checkpoint tag", err)
	}
	sql, err := exec.Command("git", "show", sha+":control-plane/migrations/schema.sql").Output()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(sql)) != checkpointChecksum {
		t.Fatal("published PostgreSQL checkpoint checksum changed")
	}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackMigration(tx)
	if _, err := tx.Exec(context.Background(), string(sql)); err != nil {
		t.Fatal(err)
	}
	if err := insertRevisionReceipt(context.Background(), tx, 1, 0, checkpointChecksum, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgreSQLArtifactPreviousCheckpoint(t *testing.T) {
	ctx := context.Background()
	upgraded, fresh := snapshotDatabase(t), snapshotDatabase(t)
	releasedCheckpoint(t, upgraded)
	if err := GrantRuntimePrivileges(ctx, upgraded, "ocservia_app"); err != nil {
		t.Fatal(err)
	}
	// Live checkpoint users' rows survive the cutover.
	if _, err := upgraded.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES('11111111-1111-1111-1111-111111111111','checkpoint preserved','checkpoint-preserved',now(),now())`); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []*pgxpool.Pool{upgraded, fresh} {
		for range 2 {
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
		}
		if err := GrantRuntimePrivileges(ctx, pool, "ocservia_app"); err != nil {
			t.Fatal(err)
		}
		a, err := parsePostgresArtifacts([]byte(schemaSQL), upgradeSQL)
		if err != nil {
			t.Fatal(err)
		}
		expected, _ := a.catalog(0)
		if err := validateStaticSchema(ctx, pool, expected); err != nil {
			t.Fatal(err)
		}
		rows, err := readRevisionReceipts(ctx, pool)
		if err != nil || len(rows) != 1 || rows[0].epoch != 2 || rows[0].revision != 0 || !matchesReceipt(rows[0], a.upgrade.Base.Checksum, a.upgrade.Base.Steps) {
			t.Fatal("new baseline differs from fresh", rows, err)
		}
		var legacy bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations') IS NOT NULL OR to_regclass('public.schema_snapshot_origin') IS NOT NULL").Scan(&legacy); err != nil || legacy {
			t.Fatal("legacy metadata retained", legacy, err)
		}
	}
	var name string
	if err := upgraded.QueryRow(ctx, "SELECT name FROM workspaces WHERE slug='checkpoint-preserved'").Scan(&name); err != nil || name != "checkpoint preserved" {
		t.Fatal("business data changed", name, err)
	}
	// Routine security and explicit grants/ACLs are independently compared.
	for _, query := range []string{
		"SELECT coalesce(jsonb_agg(jsonb_build_array(c.relname,c.relacl) ORDER BY c.relname),'[]')::text FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND NOT c.relispartition",
		"SELECT coalesce(jsonb_agg(jsonb_build_array(p.proname,pg_get_function_identity_arguments(p.oid),p.proacl) ORDER BY p.proname,pg_get_function_identity_arguments(p.oid)),'[]')::text FROM pg_proc p WHERE p.pronamespace='public'::regnamespace",
		"SELECT jsonb_agg(to_jsonb(r) ORDER BY name)::text FROM roles r",
		"SELECT jsonb_agg(to_jsonb(r) ORDER BY singleton)::text FROM controller_schema_compatibility r",
		"SELECT jsonb_agg(to_jsonb(r) ORDER BY repository)::text FROM upstream_sync_records r",
		"SELECT jsonb_agg(to_jsonb(r)-'updated_at' ORDER BY id)::text FROM scheduler_leadership r",
	} {
		var left, right *string
		if err := upgraded.QueryRow(ctx, query).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if err := fresh.QueryRow(ctx, query).Scan(&right); err != nil {
			t.Fatal(err)
		}
		if (left == nil) != (right == nil) || (left != nil && *left != *right) {
			t.Fatal("checkpoint/fresh ACL or mandatory seeds differ", query, left, right)
		}
	}
}

func TestPostgreSQLCutoverPreviousRejectsNoMutation(t *testing.T) {
	for _, tamper := range []string{
		"UPDATE schema_revisions SET epoch=4", "UPDATE schema_revisions SET revision=1",
		"UPDATE schema_revisions SET checksum=decode(repeat('00',32),'hex')",
		"UPDATE schema_revisions SET state='running',verified_at=NULL", "UPDATE schema_revisions SET step=0",
		"DELETE FROM schema_revisions", "ALTER TABLE workspaces ADD COLUMN untrusted text",
		"CREATE VIEW legacy_dependency AS SELECT version FROM schema_migrations",
	} {
		t.Run(tamper, func(t *testing.T) {
			ctx := context.Background()
			pool := snapshotDatabase(t)
			releasedCheckpoint(t, pool)
			if _, err := pool.Exec(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			before := postgresDatabaseDigest(t, pool)
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("unsupported previous state accepted")
			}
			if after := postgresDatabaseDigest(t, pool); after != before {
				t.Fatal("admission failure mutated database")
			}
		})
	}
}

func TestPostgreSQLCutoverCleanupRollback(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	releasedCheckpoint(t, pool)
	// A dependent view makes DROP without CASCADE fail. The verified epoch-2
	// receipt and every preceding transition mutation must roll back with it.
	if _, err := pool.Exec(ctx, "CREATE VIEW legacy_dependency AS SELECT version FROM schema_migrations"); err != nil {
		t.Fatal(err)
	}
	// This unpublished verification fixture includes the dependent view in its
	// expected catalog, so validation succeeds and DROP fails after the new
	// baseline receipt is written. Production admission still rejects the view.
	a, _ := parsePostgresArtifacts([]byte(schemaSQL), upgradeSQL)
	query := strings.Replace(staticFingerprintSQL, "AND NOT c.relispartition", "AND c.relname NOT IN ('schema_migrations','schema_snapshot_origin') AND NOT c.relispartition", 1)
	var catalog string
	if err := pool.QueryRow(ctx, query).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	a.upgrade.Base.Metadata, _ = json.Marshal(map[string]string{"catalog_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(catalog)))})
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := postgresDatabaseDigest(t, pool)
	err = applyArtifactRevision(ctx, conn, a, *a.upgrade.Transition, true)
	conn.Release()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "2BP01" {
		t.Fatalf("fixture did not reach dependency failure after verified new receipt: %v", err)
	}
	if after := postgresDatabaseDigest(t, pool); after != before {
		t.Fatal("failed cleanup left new receipt")
	}
}

func TestPostgreSQLCutoverConcurrentAndLock(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	releasedCheckpoint(t, pool)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- Migrate(ctx, pool) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := Migrate(waiting, pool); err == nil {
		t.Fatal("held migration lock ignored")
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal("cancelled waiter leaked session", err)
	}
}

func TestPostgreSQLCutoverUnknownDatabase(t *testing.T) {
	for _, ddl := range []string{"CREATE TABLE unknown(id int)", "CREATE VIEW lonely AS SELECT 1", "CREATE TYPE lonely AS ENUM ('one')", "CREATE COLLATION lonely (provider=libc,locale='C')", "CREATE TEXT SEARCH CONFIGURATION lonely (COPY=pg_catalog.simple)", "CREATE EXTENSION hstore", "CREATE FOREIGN DATA WRAPPER lonely", "CREATE PUBLICATION lonely", "SELECT lo_create(0)", "CREATE TABLE schema_migrations(version bigint)"} {
		t.Run(ddl, func(t *testing.T) {
			ctx := context.Background()
			pool := snapshotDatabase(t)
			if _, err := pool.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("nonempty unknown database adopted")
			}
			var journal bool
			if err := pool.QueryRow(ctx, "SELECT to_regclass('public.schema_revisions') IS NOT NULL").Scan(&journal); err != nil || journal {
				t.Fatal("refusal mutated schema", journal, err)
			}
		})
	}
}

func TestPostgreSQLSnapshotCatalogReadPermissions(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE ocservia_app"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "RESET ROLE"); err != nil {
			t.Error(err)
		}
	}()
	empty, err := databaseState(ctx, conn)
	if err != nil || !empty {
		t.Fatal("catalog requires privileged role", empty, err)
	}
}

func TestPostgreSQLCutoverArtifactTamperNoMutation(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	releasedCheckpoint(t, pool)
	before := postgresDatabaseDigest(t, pool)
	for _, upgrade := range []string{
		strings.Replace(string(upgradeSQL), checkpointRef, "v1.2.0@0000000000000000000000000000000000000000", 1),
		strings.Replace(string(upgradeSQL), checkpointChecksum, strings.Repeat("0", 64), 1),
		strings.Replace(string(upgradeSQL), "SELECT 1;", "SELECT 2;", 1),
		strings.Replace(string(upgradeSQL), "DROP TABLE public.schema_migrations", "DROP TABLE public.workspaces", 1),
	} {
		func() {
			original := upgradeSQL
			defer func() { upgradeSQL = original }()
			upgradeSQL = []byte(upgrade)
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("mutated checkpoint/transition accepted")
			}
		}()
		if after := postgresDatabaseDigest(t, pool); after != before {
			t.Fatal("artifact rejection mutated checkpoint")
		}
	}
}
