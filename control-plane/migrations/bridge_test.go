package migrations

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
)

func TestPostgreSQLBridgeCheckpoint(t *testing.T) {
	ctx := context.Background()
	known, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	current, err := loadSnapshot(known)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := baselineArtifact(current.SQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			pool := snapshotDatabase(t)
			if legacy {
				legacyReplay(t, pool, known[:len(known)-1])
			}
			for range 2 {
				if err := Migrate(ctx, pool); err != nil {
					t.Fatal(err)
				}
			}
			var count, step int
			var epoch, revision int64
			var state, sum string
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_revisions").Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			if err := pool.QueryRow(ctx, "SELECT epoch,revision,encode(checksum,'hex'),state,step FROM schema_revisions").Scan(&epoch, &revision, &sum, &state, &step); err != nil {
				t.Fatal(err)
			}
			if epoch != 1 || revision != 0 || sum != hex.EncodeToString(a.Checksum[:]) || state != "verified" || step != 1 {
				t.Fatal("incorrect checkpoint", epoch, revision, sum, state, step)
			}
			var covered int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE snapshot_covered").Scan(&covered); err != nil {
				t.Fatal(err)
			}
			if covered != 0 {
				t.Fatal("invented execution receipts", covered)
			}
			if err := GrantRuntimePrivileges(ctx, pool, "ocservia_app"); err != nil {
				t.Fatal(err)
			}
			var safe bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege('ocservia_app','schema_revisions','SELECT') AND NOT has_table_privilege('ocservia_app','schema_revisions','INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_schema_privilege('ocservia_app','public','CREATE')`).Scan(&safe); err != nil || !safe {
				t.Fatal("journal privilege", safe, err)
			}
		})
	}
}

func TestPostgreSQLBridgeRefusesUnverifiedState(t *testing.T) {
	ctx := context.Background()
	known, _ := loadMigrations()
	for _, tamper := range []string{
		"UPDATE schema_migrations SET checksum=decode(repeat('00',32),'hex') WHERE version=1",
		"DELETE FROM schema_migrations WHERE version=1",
		"ALTER TABLE workspaces ADD COLUMN foreign_column text",
		"CREATE TABLE foreign_object(id int)",
		"CREATE SCHEMA foreign_namespace",
	} {
		t.Run(tamper, func(t *testing.T) {
			pool := snapshotDatabase(t)
			legacyReplay(t, pool, known)
			if _, err := pool.Exec(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("unverified legacy checkpoint accepted")
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_revisions").Scan(&count); err != nil || count != 0 {
				t.Fatal("checkpoint stamped", count, err)
			}
		})
	}
	for _, tamper := range []string{
		"UPDATE schema_revisions SET checksum=decode(repeat('00',32),'hex')",
		"UPDATE schema_revisions SET epoch=2",
		"UPDATE schema_revisions SET revision=1",
		"UPDATE schema_revisions SET step=0",
		"UPDATE schema_revisions SET state='running',verified_at=NULL",
		"UPDATE schema_revisions SET verified_at=started_at-interval '1 second'",
	} {
		t.Run(tamper, func(t *testing.T) {
			pool := snapshotDatabase(t)
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("altered checkpoint accepted")
			}
		})
	}
}

func TestPostgreSQLBridgeFreshRollback(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	known, _ := loadMigrations()
	current, _ := loadSnapshot(known)
	_, catalog, err := baselineArtifact(current.SQL)
	if err != nil {
		t.Fatal(err)
	}
	current.SQL = strings.Replace(current.SQL, catalog, strings.Repeat("0", 64), 1)
	if err := migrate(ctx, pool, known, current, nil); err == nil {
		t.Fatal("bad schema validation succeeded")
	}
	_, empty, err := databaseState(ctx, pool)
	if err != nil || !empty {
		t.Fatal("fresh bridge was not atomic", empty, err)
	}
}
