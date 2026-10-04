package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const previousCheckpointCommit = "169102557cd610847c9f6ac2083336cdcf82c483"
const previousCheckpointRef = "v1.2.0@" + previousCheckpointCommit
const previousCheckpointChecksum = "3dc39a92ff4b54bff922872fae296843cea8dea1f2d1b36b42d86c9069dcb450"

// Historical SQL comes only from the immutable published checkpoint, never an
// archive embedded in the current executable.
func checkpointFixture(t *testing.T) (*Backend, *Backend, Options) {
	t.Helper()
	tag, err := exec.Command("git", "rev-parse", "v1.2.0^{commit}").Output()
	if err != nil || strings.TrimSpace(string(tag)) != previousCheckpointCommit {
		t.Fatal("published checkpoint tag/commit mismatch", err)
	}
	files := fstest.MapFS{}
	for _, name := range []string{"schema.sql", "upgrade.sql"} {
		data, err := exec.Command("git", "show", "v1.2.0:control-plane/internal/database/mysql/mysql/"+name).Output()
		if err != nil {
			t.Fatal("published checkpoint unavailable", err)
		}
		files["mysql/"+name] = &fstest.MapFile{Data: data}
	}
	if digest(files["mysql/schema.sql"].Data) != previousCheckpointChecksum {
		t.Fatal("immutable checkpoint changed")
	}
	old := artifactSources
	artifactSources = artifactFiles{files}
	defer func() { artifactSources = old }()
	b, admin, o := migrateFixture(t)
	var db string
	if err := b.QueryRow(context.Background(), "SELECT DATABASE()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"backend_schema_snapshot", "backend_schema_snapshot_steps", "backend_migrations", "backend_migration_steps", "backend_schema_revisions", "backend_schema_revision_steps"} {
		if _, err := b.Exec(context.Background(), "GRANT SELECT ON `"+db+"`.`"+table+"` TO 'ocservia_app'@'%'"); err != nil {
			t.Fatal(err)
		}
	}
	return b, admin, o
}

func TestMySQLCutoverCheckpointEquivalence(t *testing.T) {
	ctx := context.Background()
	old, admin, _ := checkpointFixture(t)
	fresh, _, _ := migrateFixture(t)
	for range 2 {
		if err := old.Migrate(ctx, ""); err != nil {
			t.Fatal("checkpoint transition", err)
		}
	}
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if a.schema.Epoch != 2 || a.schema.Baseline.Number != 1 || a.upgrade.Previous.Ref != previousCheckpointRef || a.upgrade.Previous.Receipt.Checksum != previousCheckpointChecksum {
		t.Fatal("checkpoint identity drift")
	}
	for _, b := range []*Backend{old, fresh} {
		if err := b.ValidateSchema(ctx); err != nil {
			t.Fatal(err)
		}
		conn, err := b.pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = a.validateShape(ctx, conn, shapeOf(a.shapes[1]), true); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if err = b.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if cutoverBusinessState(t, old) != cutoverBusinessState(t, fresh) {
		t.Fatal("fresh and upgraded schema/seeds differ")
	}
	if cutoverGrants(t, admin, old) != cutoverGrants(t, admin, fresh) {
		t.Fatal("fresh and upgraded runtime ACL differ")
	}
}

func baselineReceipts(t *testing.T, b *Backend) []string {
	t.Helper()
	conn, err := b.pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := readMySQLReceipts(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	var result []string
	for _, r := range rows {
		result = append(result, fmt.Sprintf("%d:%d:%s:%s:%d:%v:%v", r.epoch, r.revision, r.checksum, r.state, r.step, r.started, r.verified))
	}
	return result
}

func cutoverBusinessState(t *testing.T, b *Backend) string {
	t.Helper()
	ctx := context.Background()
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var result []string
	for _, s := range a.schema.Baseline.Steps {
		m := a.meta(s)
		if m.Object == "schema_revisions" {
			continue
		}
		if m.Kind == "seed" {
			_, rows, err := snapshotRows(ctx, conn, m.Object, m.Columns)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(rows)
			result = append(result, m.Object+string(data))
		} else {
			hash, err := a.postcondition(ctx, conn, m)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, objectKey(m)+hash)
		}
	}
	sort.Strings(result)
	return strings.Join(result, "\n")
}
func cutoverGrants(t *testing.T, admin, b *Backend) string {
	t.Helper()
	ctx := context.Background()
	var db string
	if err := b.QueryRow(ctx, "SELECT DATABASE()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	rows, err := admin.Query(ctx, "SHOW GRANTS FOR 'ocservia_app'@'%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var grants []string
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(grant, "`"+db+"`") {
			grants = append(grants, strings.ReplaceAll(grant, "`"+db+"`", "`database`"))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(grants)
	return strings.Join(grants, "\n")
}
func TestMySQLCutoverUnsupportedNoMutation(t *testing.T) {
	ctx := context.Background()
	for _, query := range []string{
		"UPDATE schema_revisions SET epoch=3", "UPDATE schema_revisions SET revision=1", "UPDATE schema_revisions SET checksum=REPEAT('0',64)", "UPDATE schema_revisions SET step=1", "UPDATE schema_revisions SET state='running',verified_at=NULL", "DELETE FROM schema_revisions", "ALTER TABLE workspaces ADD COLUMN foreign_column INT", "ALTER TABLE backend_migrations ADD COLUMN foreign_column INT",
	} {
		t.Run(query, func(t *testing.T) {
			b, _, _ := checkpointFixture(t)
			if _, err := b.Exec(ctx, query); err != nil {
				t.Fatal(err)
			}
			before := mysqlArtifactDigest(t, b)
			if err := b.Migrate(ctx, ""); err == nil {
				t.Fatal("unsupported previous state accepted")
			}
			if before != mysqlArtifactDigest(t, b) {
				t.Fatal("unsupported transition mutated database")
			}
		})
	}
	for _, ddl := range []string{"CREATE TABLE unknown(id INT)", "CREATE VIEW unknown AS SELECT 1", "CREATE PROCEDURE unknown() SELECT 1", "CREATE EVENT unknown ON SCHEDULE EVERY 1 DAY DO SELECT 1"} {
		t.Run(ddl, func(t *testing.T) {
			b, _, _ := fixture(t)
			if _, err := b.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			before := mysqlArtifactDigest(t, b)
			if err := b.Migrate(ctx, ""); err == nil {
				t.Fatal("unknown nonempty database adopted")
			}
			if before != mysqlArtifactDigest(t, b) {
				t.Fatal("unknown refusal mutated database")
			}
		})
	}
}
func TestMySQLCutoverCrashRecovery(t *testing.T) {
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	publish := "UPDATE schema_revisions SET checksum=?,step=?,state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE epoch=? AND revision=? AND state='running'"
	type boundary struct {
		name, command string
		forward       bool
		occurrence    int32
		clean         bool
		revision      int64
	}
	cases := []boundary{
		{"transition-before-statement", string(a.upgrade.Transition.Steps[0].SQL), false, 1, false, 0},
		{"transition-before-verified", publish, false, 1, false, 0},
		{"transition-after-verified", publish, true, 1, true, 0},
		{"cleanup-before-statement", string(a.upgrade.Revisions[0].Steps[0].SQL), false, 1, false, 1},
		{"cleanup-after-progress", "UPDATE schema_revisions SET step=? WHERE epoch=? AND revision=? AND state='running' AND step=?", true, 2, false, 1},
		{"cleanup-before-verified", publish, false, 2, false, 1},
		{"cleanup-after-verified", publish, true, 2, true, 1},
	}
	for _, s := range a.upgrade.Revisions[0].Steps {
		cases = append(cases, boundary{"cleanup-after-ddl-" + s.Name, string(s.SQL), true, 1, false, 1})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			b, _, o := checkpointFixture(t)
			cfg, err := driver.ParseDSN(o.DSN)
			if err != nil {
				t.Fatal(err)
			}
			proxy := newFinalizeProxy(t, cfg.Addr, test.command, test.forward)
			proxy.occurrence.Store(test.occurrence)
			cfg.Addr = proxy.listener.Addr().String()
			child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=90s")
			child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_DSN="+cfg.FormatDSN())
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			defer child.Process.Kill()
			select {
			case <-proxy.hit:
			case <-time.After(40 * time.Second):
				child.Process.Kill()
				child.Wait()
				t.Fatal("process did not reach boundary")
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			child.Wait()
			proxy.close()
			ctx := context.Background()
			if test.revision == 1 {
				var verified bool
				if err = b.QueryRow(ctx, "SELECT state='verified' FROM schema_revisions WHERE epoch=2 AND revision=0").Scan(&verified); err != nil || !verified {
					t.Fatal("legacy cleanup preceded verified new checkpoint", err)
				}
			}
			if !test.clean {
				before := mysqlArtifactDigest(t, b)
				if err = b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
					t.Fatal("interrupted work resumed without approval", err)
				}
				if err = b.Migrate(ctx, "wrong"); !errors.Is(err, ErrChecksum) {
					t.Fatal(err)
				}
				if before != mysqlArtifactDigest(t, b) {
					t.Fatal("wrong repair mutated database")
				}
				sum, err := ArtifactChecksum(MySQL, "upgrade", test.revision)
				if err != nil {
					t.Fatal(err)
				}
				if err = b.Migrate(ctx, sum); err != nil {
					t.Fatal("exact artifact repair", err)
				}
			} else if err = b.Migrate(ctx, ""); err != nil {
				t.Fatal("verified restart", err)
			}
			if err = b.ValidateSchema(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealSchemaDriftRepairRefused(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	if _, err := b.Exec(ctx, "ALTER TABLE workspaces ADD COLUMN foreign_column INT"); err != nil {
		t.Fatal(err)
	}
	before := mysqlArtifactDigest(t, b)
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrSchema) {
		t.Fatal("current schema drift accepted", err)
	}
	sum, err := ArtifactChecksum(MySQL, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, sum); !errors.Is(err, ErrChecksum) {
		t.Fatal("verified schema forcibly repaired", err)
	}
	if before != mysqlArtifactDigest(t, b) {
		t.Fatal("rejected repair mutated schema/journal")
	}
}

func verifiedCutoverBaseline(t *testing.T, b *Backend) mysqlArtifacts {
	t.Helper()
	ctx := context.Background()
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMigrationConnection(conn, lock)
	var shape map[string]step
	if err = json.Unmarshal(a.upgrade.Previous.Receipt.Metadata, &shape); err != nil {
		t.Fatal(err)
	}
	if err = a.validateShape(ctx, conn, shape, true); err != nil {
		t.Fatal(err)
	}
	rev := a.upgrade.Transition
	sum := fmt.Sprintf("%x", rev.Checksum)
	if err = startMySQLRevision(ctx, conn, 2, 0, sum); err != nil {
		t.Fatal(err)
	}
	r := mysqlReceipt{epoch: 2, revision: 0, checksum: sum, state: "running"}
	if err = a.executeSteps(ctx, conn, r, rev.Steps, shape, false); err != nil {
		t.Fatal(err)
	}
	if err = publishMySQLRevision(ctx, conn, r, sum, len(rev.Steps)); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMySQLCutoverForeignCleanupRepair(t *testing.T) {
	b, _, _ := checkpointFixture(t)
	a := verifiedCutoverBaseline(t, b)
	ctx := context.Background()
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rev := a.upgrade.Revisions[0]
	sum := fmt.Sprintf("%x", rev.Checksum)
	if err = startMySQLRevision(ctx, conn, 2, 1, sum); err != nil {
		t.Fatal(err)
	}
	first := a.meta(rev.Steps[0])
	if _, err = conn.ExecContext(ctx, string(rev.Steps[0].SQL)); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "CREATE TABLE `"+first.Object+"`(foreign_column INT)"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	before := mysqlArtifactDigest(t, b)
	if err = b.Migrate(ctx, sum); !errors.Is(err, ErrSchema) {
		t.Fatal("foreign replacement scheduled for DROP", err)
	}
	if before != mysqlArtifactDigest(t, b) {
		t.Fatal("foreign replacement mutated on rejected repair")
	}
}

func TestMySQLCutoverTransitionArtifactTamper(t *testing.T) {
	b, _, _ := checkpointFixture(t)
	verifiedCutoverBaseline(t, b)
	schema, err := artifactSources.ReadFile("mysql/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	upgrade, err := artifactSources.ReadFile("mysql/upgrade.sql")
	if err != nil {
		t.Fatal(err)
	}
	upgrade = []byte(strings.Replace(string(upgrade), "SELECT 1;\n", "SELECT 2;\n", 1))
	useMySQLArtifacts(t, schema, upgrade)
	before := mysqlArtifactDigest(t, b)
	if err = b.Migrate(context.Background(), ""); !errors.Is(err, ErrChecksum) {
		t.Fatal("verified transition SQL changed", err)
	}
	if before != mysqlArtifactDigest(t, b) {
		t.Fatal("transition artifact tamper mutated database")
	}
}

func TestMySQLCutoverArtifactIntegrity(t *testing.T) {
	schema, err := artifactSources.ReadFile("mysql/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	upgrade, err := artifactSources.ReadFile("mysql/upgrade.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(string(upgrade), "DROP TABLE `backend_migrations`;", "DROP  TABLE `backend_migrations`;", 1),
		strings.Replace(string(upgrade), "-- ocservia:revision=1", "-- ocservia:revision=2", 1),
		strings.Split(string(upgrade), "-- ocservia:revision=1")[0],
		strings.Replace(string(upgrade), "-- ocservia:end-step", "-- ocservia:end-transition", 1),
		strings.Replace(string(upgrade), "-- ocservia:step=002:", "-- ocservia:step=001:", 1),
	} {
		if _, err := parseMySQLArtifacts(schema, []byte(bad)); !errors.Is(err, ErrChecksum) {
			t.Fatal("malformed/mutated artifact accepted", err)
		}
	}
}
