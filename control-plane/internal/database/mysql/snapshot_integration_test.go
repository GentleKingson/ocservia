package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	driver "github.com/go-sql-driver/mysql"
	"io/fs"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestSnapshotLifecycle(t *testing.T) {
	ctx := context.Background()
	chain, err := loadRevisionChain(MySQL)
	if err != nil {
		t.Fatal(err)
	}
	a, err := currentSnapshot(chain)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("no-fabricated-history", func(t *testing.T) {
		b, _, _ := migrateFixture(t)
		for _, table := range []string{"backend_migrations", "backend_migration_steps", "backend_schema_revisions", "backend_schema_revision_steps", "time_migration_decisions"} {
			var n int
			if err := b.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
				t.Fatal(table, n, err)
			}
		}
		if err := b.PrepareControllerTelemetry(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.ValidateSchema(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown-nonempty", func(t *testing.T) {
		for _, ddl := range []string{"CREATE TABLE unrelated(id INT)", "CREATE VIEW unrelated AS SELECT 1 AS id", "CREATE PROCEDURE unrelated() SELECT 1", "CREATE EVENT unrelated ON SCHEDULE EVERY 1 DAY DO SELECT 1", snapshotOriginDDL, metadataDDL} {
			t.Run(ddl, func(t *testing.T) {
				b, _, _ := fixture(t)
				if _, err := b.Exec(ctx, ddl); err != nil {
					t.Fatal(err)
				}
				conn, err := b.pool.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				before, _ := databaseObjectCount(ctx, conn)
				conn.Close()
				if err = b.Migrate(ctx, ""); err == nil {
					t.Fatal("unknown database adopted")
				}
				conn, err = b.pool.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				after, _ := databaseObjectCount(ctx, conn)
				conn.Close()
				if before != after {
					t.Fatal("refusal wrote metadata")
				}
			})
		}
	})
	t.Run("receipt-tampering", func(t *testing.T) {
		for _, query := range []string{"UPDATE backend_schema_snapshot SET artifact_checksum=REPEAT('0',64)", "UPDATE backend_schema_snapshot_steps SET checksum=REPEAT('0',64) WHERE ordinal=3", "UPDATE backend_schema_snapshot_steps SET name='tampered' WHERE ordinal=3", "INSERT INTO backend_schema_revisions(version,parent_checksum,manifest_checksum,state) VALUES(2,REPEAT('a',64),REPEAT('b',64),'verified')"} {
			t.Run(query, func(t *testing.T) {
				b, _, _ := migrateFixture(t)
				if _, err := b.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
				if err := b.Migrate(ctx, ""); !errors.Is(err, ErrChecksum) {
					t.Fatal("tampered receipt accepted", err)
				}
			})
		}
	})
	t.Run("interruption-and-exact-repair", func(t *testing.T) {
		for _, phase := range []string{"inception", "origin", "journal", "before-ddl", "after-ddl", "before-publish"} {
			t.Run(phase, func(t *testing.T) {
				b, _, _ := fixture(t)
				if phase == "before-publish" {
					if err := b.Migrate(ctx, ""); err != nil {
						t.Fatal(err)
					}
					if _, err := b.Exec(ctx, "UPDATE backend_schema_snapshot SET state='running',verified_at=NULL"); err != nil {
						t.Fatal(err)
					}
				} else {
					partialSnapshot(t, b, a, phase)
				}
				if err := b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
					t.Fatal("interrupted snapshot silently accepted", err)
				}
				if err := b.Migrate(ctx, "wrong"); !errors.Is(err, ErrChecksum) {
					t.Fatal("wrong artifact accepted", err)
				}
				if err := b.Migrate(ctx, a.sum); err != nil {
					t.Fatal("repair", err)
				}
				if err := b.Migrate(ctx, ""); err != nil {
					t.Fatal("repeat", err)
				}
				if err := b.ValidateSchema(ctx); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("repair-refuses-drift", func(t *testing.T) {
		b, _, _ := fixture(t)
		partialSnapshot(t, b, a, "after-ddl")
		if _, err := b.Exec(ctx, "ALTER TABLE `"+a.Statements[2].Name+"` ADD COLUMN alien INT"); err != nil {
			t.Fatal(err)
		}
		if err := b.Migrate(ctx, a.sum); !errors.Is(err, ErrSchema) {
			t.Fatal(err)
		}
	})
}

func partialSnapshot(t *testing.T, b *Backend, a snapshotArtifact, phase string) {
	t.Helper()
	ctx := context.Background()
	exec := func(query string, args ...any) {
		if _, err := b.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(snapshotSQL(a, a.Statements[0]) + " COMMENT='" + snapshotCommentPrefix + a.sum + "'")
	if phase == "inception" {
		return
	}
	exec("INSERT INTO backend_schema_snapshot(singleton,artifact_checksum,state) VALUES(1,?,'running')", a.sum)
	if phase == "origin" {
		return
	}
	exec(snapshotSQL(a, a.Statements[1]))
	if phase == "journal" {
		return
	}
	for i := 0; i < 2; i++ {
		s := a.Statements[i]
		exec("INSERT INTO backend_schema_snapshot_steps(ordinal,name,checksum,state,verified_at) VALUES(?,?,?,'verified',CURRENT_TIMESTAMP(6))", i+1, s.Kind+":"+s.Name, s.Checksum)
	}
	s := a.Statements[2]
	exec("INSERT INTO backend_schema_snapshot_steps(ordinal,name,checksum,state) VALUES(3,?,?,'running')", s.Kind+":"+s.Name, s.Checksum)
	if phase == "after-ddl" {
		exec(snapshotSQL(a, s))
	}
}

func TestSnapshotForwardRevision(t *testing.T) {
	ctx := context.Background()
	chain, err := loadRevisionChain(MySQL)
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentSnapshot(chain)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := fs.Glob(manifests, "mysql/snapshots/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var old snapshotArtifact
	for _, path := range paths {
		data, err := manifests.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := decodeSnapshot(data, chain)
		if err == nil && candidate.Covered == 30 {
			old = candidate
			break
		}
	}
	if old.Covered != 30 {
		t.Fatal("previous checkpoint descriptor missing")
	}
	// Recover the original schema byte ranges from the current marker artifact,
	// then prove them against the immutable previous descriptor's schema digest.
	for _, step := range current.Statements {
		if step.Kind == "table" && step.Name == "schema_revisions" {
			continue
		}
		old.sql = append(old.sql, []byte(snapshotSQL(current, step)+";\n")...)
	}
	if digest(old.sql) != old.SchemaChecksum {
		t.Fatal("previous release SQL bytes differ")
	}
	b, _, _ := fixture(t)
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if err = initializeSnapshot(ctx, conn, old, nil, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err = releaseMigrationConnection(conn, lock); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = b.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err = b.ValidateSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var actual string
	if err = b.QueryRow(ctx, "SELECT artifact_checksum FROM backend_schema_snapshot WHERE singleton=1").Scan(&actual); err != nil || actual != old.sum {
		t.Fatal("previous origin rewritten", actual, err)
	}
	var count int
	if err = b.QueryRow(ctx, "SELECT COUNT(*) FROM backend_schema_revisions WHERE version=31 AND state='verified'").Scan(&count); err != nil || count != 1 {
		t.Fatal("bridge not executed", count, err)
	}
	if err = b.QueryRow(ctx, "SELECT COUNT(*) FROM schema_revisions WHERE epoch=1 AND revision=0 AND state='verified'").Scan(&count); err != nil || count != 1 {
		t.Fatal("checkpoint missing", count, err)
	}
}

func TestSnapshotLegacyLineages(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(map[bool]string{true: "historical", false: "baseline"}[old], func(t *testing.T) {
			b, _ := historicalFixture(t, old)
			before := baselineReceipts(t, b)
			if err := b.Migrate(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, baselineReceipts(t, b)) {
				t.Fatal("real historical receipts changed")
			}
			var n int
			if err := b.QueryRow(context.Background(), "SELECT COUNT(*) FROM backend_schema_snapshot").Scan(&n); err != nil || n != 0 {
				t.Fatal("legacy snapshot origin fabricated", n, err)
			}
			if _, err := b.Exec(context.Background(), "UPDATE backend_migration_steps SET name='tampered' WHERE ordinal=1"); err != nil {
				t.Fatal(err)
			}
			if err := b.Migrate(context.Background(), ""); !errors.Is(err, ErrChecksum) {
				t.Fatal("legacy name tamper accepted", err)
			}
		})
	}
}

func TestSnapshotArtifactValidation(t *testing.T) {
	chain, err := loadRevisionChain(MySQL)
	if err != nil {
		t.Fatal(err)
	}
	a, err := currentSnapshot(chain)
	if err != nil {
		t.Fatal(err)
	}
	original := manifests
	descriptor, err := original.ReadFile("mysql/schema.snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"mysql/schema.sql", "mysql/schema.snapshot.json"} {
		catalog := fstest.MapFS{"mysql/schema.sql": &fstest.MapFile{Data: append([]byte(nil), a.sql...)}, "mysql/schema.snapshot.json": &fstest.MapFile{Data: append([]byte(nil), descriptor...)}}
		catalog[path].Data = append(catalog[path].Data, ' ')
		before := append([]byte(nil), catalog[path].Data...)
		manifests = artifactFiles{catalog}
		err = checkSnapshotArtifacts(a, descriptor)
		manifests = original
		if !errors.Is(err, ErrChecksum) || !reflect.DeepEqual(before, catalog[path].Data) {
			t.Fatal("read-only check accepted or rewrote drift", path, err)
		}
	}
	for _, change := range []func(*schemaSnapshot){func(s *schemaSnapshot) { s.HistoryChecksum = strings.Repeat("0", 64) }, func(s *schemaSnapshot) { s.RevisionChecksum = strings.Repeat("0", 64) }, func(s *schemaSnapshot) { s.Statements[2].Offset++ }, func(s *schemaSnapshot) { s.Statements[0].Length = int(^uint(0) >> 1) }, func(s *schemaSnapshot) { s.Parent = strings.Repeat("0", 64) }} {
		copy := a.schemaSnapshot
		copy.Statements = append([]schemaStatement(nil), a.Statements...)
		change(&copy)
		data, _ := json.Marshal(copy)
		if _, err := decodeSnapshot(data, chain); !errors.Is(err, ErrChecksum) {
			t.Fatal("descriptor mutation accepted", err)
		}
	}
}

func TestSnapshotRuntimePermissions(t *testing.T) {
	ctx := context.Background()
	b, _, o := migrateFixture(t)
	if err := b.PrepareControllerTelemetry(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	config, err := driver.ParseDSN(o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.User = "ocservia_app"
	config.Passwd = "pr02-runtime-test-only"
	o.DSN = config.FormatDSN()
	runtime, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	// Controller runtime checks connectivity/permissions and telemetry; complete
	// static schema validation remains owner-only (including migration evidence).
	if err = runtime.CheckReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	if err = runtime.ValidateTelemetryRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"UPDATE backend_schema_snapshot SET state='verified'", "DELETE FROM backend_schema_snapshot_steps", "CREATE TABLE forbidden(id INT)"} {
		if _, err = runtime.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
			t.Fatal(query, err)
		}
	}
}

func TestSnapshotCrashAfterDDL(t *testing.T) {
	b, _, o := fixture(t)
	ctx := context.Background()
	chain, _ := loadRevisionChain(MySQL)
	a, err := currentSnapshot(chain)
	if err != nil {
		t.Fatal(err)
	}
	config, err := driver.ParseDSN(o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	statement := a.Statements[2]
	proxy := newFinalizeProxy(t, config.Addr, snapshotSQL(a, statement), true)
	config.Addr = proxy.listener.Addr().String()
	child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=90s")
	child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_CRASH_LEGACY=no", "PR02_DSN="+config.FormatDSN())
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	select {
	case <-proxy.hit:
	case <-time.After(20 * time.Second):
		child.Process.Kill()
		child.Wait()
		t.Fatal("child did not reach committed snapshot DDL")
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	proxy.close()
	var state string
	if err = b.QueryRow(ctx, "SELECT state FROM backend_schema_snapshot_steps WHERE ordinal=3").Scan(&state); err != nil || state != "running" {
		t.Fatal("DDL completion silently verified", state, err)
	}
	if err = b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, a.sum); err != nil {
		t.Fatal(err)
	}
	if err = b.ValidateSchema(ctx); err != nil {
		t.Fatal(err)
	}
}
