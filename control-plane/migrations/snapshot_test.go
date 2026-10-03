package migrations

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentSnapshotSource(t *testing.T) {
	known, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadSnapshot(known); err != nil {
		t.Fatal(err)
	}
	changed := append([]Migration(nil), known...)
	changed[0].Checksum = sha256.Sum256([]byte("changed"))
	if _, err := loadSnapshot(changed); err == nil {
		t.Fatal("altered historical source accepted")
	}
	original := snapshotSQL
	t.Cleanup(func() { snapshotSQL = original })
	snapshotSQL += "\nSELECT 1;"
	if _, err := loadSnapshot(known); err == nil {
		t.Fatal("altered snapshot accepted")
	}
}

func snapshotDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("OCSERV_TEST_SNAPSHOT_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL snapshot database required")
	}
	ctx := context.Background()
	admin, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "snapshot_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	return pool
}

func legacyReplay(t *testing.T, pool *pgxpool.Pool, known []Migration) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations(version bigint PRIMARY KEY,name text NOT NULL,checksum bytea NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, m := range known {
		if err := applyMigration(ctx, conn, m, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgreSQLSnapshotLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	known, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadSnapshot(known)
	if err != nil {
		t.Fatal(err)
	}
	// New databases do not execute historical preflights or migration SQL.
	forbid := func(context.Context, pgx.Tx, int64) error { return fmt.Errorf("historical replay forbidden") }
	if err := Migrate(ctx, pool, forbid); err != nil {
		t.Fatal(err)
	}
	var covered int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE snapshot_covered").Scan(&covered); err != nil || covered != 0 {
		t.Fatal(covered, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES('11111111-1111-1111-1111-111111111111','snapshot preserved','snapshot-preserved',now(),now())`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, forbid); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntimePrivileges(ctx, pool, "ocservia_app"); err != nil {
		t.Fatal(err)
	}
	var allowed bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege('ocservia_app','workspaces','SELECT') AND NOT has_table_privilege('ocservia_app','schema_migrations','INSERT') AND NOT has_function_privilege('ocservia_app','telemetry_ensure_month_partition(timestamptz)','EXECUTE')`).Scan(&allowed); err != nil || !allowed {
		t.Fatal("runtime privilege boundary", allowed, err)
	}
	// The bridge is the last legacy migration. New forward revisions belong
	// to upgrade.sql and must not silently change the checkpoint checksum.
	var checkpoints int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_revisions WHERE epoch=1 AND revision=0 AND state='verified'").Scan(&checkpoints); err != nil || checkpoints != 1 {
		t.Fatal("trusted checkpoint missing", checkpoints, err)
	}
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM workspaces WHERE slug='snapshot-preserved'").Scan(&name); err != nil || name != "snapshot preserved" {
		t.Fatal(name, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE schema_revisions SET checksum=decode(repeat('00',32),'hex')"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("altered historical checksum accepted")
	}
}

func TestPostgreSQLSnapshotRejectsUnprovenState(t *testing.T) {
	for _, ddl := range []string{
		"CREATE TABLE empty_business(id int)",
		"CREATE VIEW lonely_view AS SELECT 1 AS value",
		"CREATE FUNCTION lonely_function() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE TYPE lonely_type AS ENUM ('one')",
		"CREATE COLLATION lonely_collation (provider = libc, locale = 'C')",
		"CREATE TEXT SEARCH CONFIGURATION lonely_search (COPY = pg_catalog.simple)",
		"CREATE EXTENSION hstore",
		"CREATE FOREIGN DATA WRAPPER lonely_wrapper",
		"CREATE PUBLICATION lonely_publication",
		"SELECT lo_create(0)",
		"CREATE TABLE schema_migrations(version bigint PRIMARY KEY,name text,checksum bytea)",
	} {
		t.Run(ddl, func(t *testing.T) {
			pool := snapshotDatabase(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("unproven database adopted")
			}
			var n int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relname='workspaces'").Scan(&n); err != nil || n != 0 {
				t.Fatal("schema was initialized", n, err)
			}
		})
	}
}

func TestPostgreSQLSnapshotOriginTampering(t *testing.T) {
	for _, statement := range []string{
		"UPDATE schema_snapshot_origin SET receipt_sha256=decode(repeat('00',32),'hex')",
		"UPDATE schema_snapshot_origin SET history_sha256=decode(repeat('00',32),'hex')",
		"UPDATE schema_snapshot_origin SET schema_sha256=decode(repeat('00',32),'hex')",
		"DELETE FROM schema_snapshot_origin",
		"DELETE FROM schema_migrations WHERE version=1",
		"INSERT INTO schema_migrations(version,name,checksum,snapshot_covered) VALUES(9000001,'unknown',decode(repeat('01',32),'hex'),true); UPDATE schema_migrations SET snapshot_covered=false WHERE version=1",
		"UPDATE schema_migrations SET name='changed' WHERE version=1",
	} {
		t.Run(statement, func(t *testing.T) {
			pool := snapshotDatabase(t)
			ctx := context.Background()
			known, _ := loadMigrations()
			current, _ := loadSnapshot(known)
			if err := migrate(ctx, pool, known, current, nil); err != nil {
				t.Fatal(err)
			}
			// The fallback must validate legacy provenance before establishing
			// a checkpoint. New artifacts never fabricate these coverage rows.
			if _, err := pool.Exec(ctx, "DELETE FROM schema_revisions"); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, statement); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("tampered provenance accepted")
			}
		})
	}
}

func TestPostgreSQLSnapshotAtomicityAndLock(t *testing.T) {
	pool := snapshotDatabase(t)
	ctx := context.Background()
	known, _ := loadMigrations()
	current, _ := loadSnapshot(known)
	bad := current
	bad.SQL += "\nSELECT 1/0;"
	if err := migrate(ctx, pool, known, bad, nil); err == nil {
		t.Fatal("broken snapshot succeeded")
	}
	_, empty, err := databaseState(ctx, pool)
	if err != nil || !empty {
		t.Fatal("snapshot did not roll back", empty, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- Migrate(ctx, pool) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
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
		t.Fatal("migration ignored held lock")
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal("lock cancellation leaked connection lock", err)
	}
}

func TestPostgreSQLLegacyUpgrade(t *testing.T) {
	pool := snapshotDatabase(t)
	ctx := context.Background()
	known, _ := loadMigrations()
	legacyReplay(t, pool, known[:len(known)-1])
	var original time.Time
	if err := pool.QueryRow(ctx, "SELECT applied_at FROM schema_migrations WHERE version=1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES('11111111-1111-1111-1111-111111111111','legacy preserved','legacy-preserved',now(),now())`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := Migrate(ctx, pool, func(_ context.Context, _ pgx.Tx, v int64) error {
		calls++
		if v != known[len(known)-1].Version {
			return fmt.Errorf("replayed history %d", v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	var after time.Time
	var origins int
	if err := pool.QueryRow(ctx, "SELECT applied_at FROM schema_migrations WHERE version=1").Scan(&after); err != nil || !original.Equal(after) {
		t.Fatal("historical timestamp rewritten", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_snapshot_origin").Scan(&origins); err != nil || origins != 0 {
		t.Fatal("legacy falsely marked snapshot", origins, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func TestPostgreSQLLegacyHistoryGapRejected(t *testing.T) {
	pool := snapshotDatabase(t)
	ctx := context.Background()
	known, _ := loadMigrations()
	legacyReplay(t, pool, known[:len(known)-1])
	if _, err := pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version=24"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("legacy history gap not rejected: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE version=24 OR version=$1", known[len(known)-1].Version).Scan(&count); err != nil || count != 0 {
		t.Fatal("history was rewritten or migration applied after gap", count, err)
	}
}

func TestPostgreSQLSnapshotCatalogReadPermissions(t *testing.T) {
	pool := snapshotDatabase(t)
	ctx := context.Background()
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
	_, empty, err := databaseState(ctx, conn)
	if err != nil || !empty {
		t.Fatal("catalog classification requires privileged account", empty, err)
	}
}
