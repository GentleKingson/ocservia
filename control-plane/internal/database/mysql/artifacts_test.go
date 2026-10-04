package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
	driver "github.com/go-sql-driver/mysql"
)

func useMySQLArtifacts(t *testing.T, schema, upgrade []byte) {
	t.Helper()
	original := artifactSources
	t.Cleanup(func() { artifactSources = original })
	// New execution and validation must succeed with no JSON/history files.
	artifactSources = artifactFiles{fstest.MapFS{"mysql/schema.sql": {Data: schema}, "mysql/upgrade.sql": {Data: upgrade}}}
}
func artifactStepText(n int, name string, m artifactStepMetadata, statement string) string {
	data, _ := json.Marshal(m)
	return fmt.Sprintf("-- ocservia:step=%03d:%s\n-- ocservia:metadata=%s\n%s-- ocservia:end-step\n", n, name, data, statement)
}
func pinMySQLFuture(t *testing.T, schema, upgrade []byte) ([]byte, []byte) {
	t.Helper()
	linesSchema := strings.Split(string(schema), "\n")
	kept := linesSchema[:0]
	for _, line := range linesSchema {
		if !strings.HasPrefix(line, "-- ocservia:history-sha256=") {
			kept = append(kept, line)
		}
	}
	schema = []byte(strings.Join(kept, "\n"))
	parsed, err := schemaartifact.Parse(upgrade, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	history, err := schemaartifact.HistoryChecksum(parsed, int64(len(parsed.Revisions)))
	if err != nil {
		t.Fatal(err)
	}
	schema = []byte(strings.Replace(string(schema), fmt.Sprintf("-- ocservia:revision=%d\n", len(parsed.Revisions)), fmt.Sprintf("-- ocservia:revision=%d\n-- ocservia:history-sha256=", len(parsed.Revisions))+history+"\n", 1))
	cp := schemaartifact.Receipt{Checksum: digest(schema), Steps: strings.Count(string(schema), "-- ocservia:step=")}
	receipt, _ := json.Marshal(cp)
	lines := strings.Split(string(upgrade), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "-- ocservia:checkpoint=") {
			lines[i] = "-- ocservia:checkpoint=" + string(receipt)
			break
		}
	}

	return schema, []byte(strings.Join(lines, "\n"))
}
func futureMySQLArtifacts(t *testing.T) ([]byte, []byte) {
	t.Helper()
	ctx := context.Background()
	reference, _, _ := migrateFixture(t)
	base, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := artifactSources.ReadFile("mysql/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	upgrade, err := artifactSources.ReadFile("mysql/upgrade.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := "CREATE TABLE artifact_probe(id INT PRIMARY KEY, value INT NOT NULL) ENGINE=InnoDB;\n"
	if _, err = reference.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	conn, err := reference.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	after, err := schemaHashWithFingerprint(ctx, conn, step{Name: "artifact_probe", Kind: "table"}, base.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reference.Exec(ctx, "INSERT INTO artifact_probe VALUES(1,2)"); err != nil {
		t.Fatal(err)
	}
	_, values, err := snapshotRows(ctx, conn, "artifact_probe", []string{"id", "value"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(values)
	table := artifactStepMetadata{Kind: "table", Object: "artifact_probe", After: after}
	seed := artifactStepMetadata{Kind: "seed", Object: "artifact_probe", After: digest(data), Columns: []string{"id", "value"}}
	schema = []byte(strings.Replace(string(schema), "-- ocservia:revision=1", "-- ocservia:revision=2", 1) + artifactStepText(len(base.schema.Baseline.Steps)+1, "probe_table", table, ddl) + artifactStepText(len(base.schema.Baseline.Steps)+2, "probe_seed", seed, "INSERT INTO artifact_probe VALUES(1,2);\n"))
	cp, _ := json.Marshal(schemaartifact.Receipt{Checksum: digest(schema), Steps: len(base.schema.Baseline.Steps) + 2})
	insert := artifactStepMetadata{Kind: "data", Object: "artifact_probe", After: digest([]byte("valid")), VerifySQL: "SELECT IF(COUNT(*)=1,'valid','invalid') FROM artifact_probe WHERE id=1 AND value=1"}
	update := artifactStepMetadata{Kind: "data", Object: "artifact_probe", After: digest([]byte("valid")), VerifySQL: "SELECT IF(COUNT(*)=1,'valid','invalid') FROM artifact_probe WHERE id=1 AND value=2"}
	upgrade = append(upgrade, []byte("\n-- ocservia:revision=2\n-- ocservia:checkpoint="+string(cp)+"\n"+artifactStepText(1, "probe_create", table, ddl)+artifactStepText(2, "probe_insert", insert, "INSERT INTO artifact_probe VALUES(1,1);\n")+artifactStepText(3, "probe_update", update, "UPDATE artifact_probe SET value=value+1 WHERE id=1;\n")+"-- ocservia:end-revision\n")...)
	return pinMySQLFuture(t, schema, upgrade)
}
func mysqlArtifactDigest(t *testing.T, b *Backend) string {
	t.Helper()
	ctx := context.Background()
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var all []string
	rows, err := conn.QueryContext(ctx, "SELECT TABLE_NAME,TABLE_TYPE,COALESCE(TABLE_COMMENT,'') FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() ORDER BY TABLE_NAME")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n, k, c string
		if err = rows.Scan(&n, &k, &c); err != nil {
			t.Fatal(err)
		}
		all = append(all, n+k+c)
	}
	rows.Close()
	for _, line := range append([]string(nil), all...) {
		name := strings.Split(line, "BASE TABLE")[0]
		if strings.Contains(line, "BASE TABLE") {
			var n, ddl string
			if err = conn.QueryRowContext(ctx, "SHOW CREATE TABLE `"+name+"`").Scan(&n, &ddl); err != nil {
				t.Fatal(err)
			}
			all = append(all, ddl)
		}
	}
	for _, table := range []string{"schema_revisions", "artifact_probe"} {
		var exists int
		if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists == 0 {
			continue
		}
		_, values, err := snapshotRows(ctx, conn, table, nil)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(values)
		all = append(all, string(encoded))
	}
	return strings.Join(all, "\n")
}
func TestMySQLArtifactFreshAndUpgrade(t *testing.T) {
	ctx := context.Background()
	old, _, _ := migrateFixture(t)
	schema, upgrade := futureMySQLArtifacts(t)
	useMySQLArtifacts(t, schema, upgrade)
	if _, err := loadMySQLArtifacts(); err != nil {
		t.Fatal("fixture contract", err)
	}
	for range 2 {
		if err := old.Migrate(ctx, ""); err != nil {
			t.Fatal("upgrade", err)
		}
	}
	fresh, _, _ := migrateFixture(t)
	for _, b := range []*Backend{old, fresh} {
		if err := b.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := b.PrepareControllerTelemetry(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.ValidateSchema(ctx); err != nil {
			t.Fatal(err)
		}
		var value int
		if err := b.QueryRow(ctx, "SELECT value FROM artifact_probe WHERE id=1").Scan(&value); err != nil || value != 2 {
			t.Fatal("data replayed or missing", value, err)
		}
	}
	for b, want := range map[*Backend]int{old: 2, fresh: 1} {
		var count int
		if err := b.QueryRow(ctx, "SELECT COUNT(*) FROM schema_revisions").Scan(&count); err != nil || count != want {
			t.Fatal("fabricated/missing receipts", count, want, err)
		}

	}
}
func TestMySQLArtifactUnsupportedNoMutation(t *testing.T) {
	ctx := context.Background()
	b, _, _ := migrateFixture(t)
	for _, tamper := range []string{"UPDATE schema_revisions SET epoch=3", "UPDATE schema_revisions SET revision=42", "UPDATE schema_revisions SET checksum=REPEAT('0',64)", "UPDATE schema_revisions SET step=0", "UPDATE schema_revisions SET state='running',verified_at=NULL", "INSERT INTO schema_revisions SELECT epoch,3,checksum,state,step,started_at,verified_at FROM schema_revisions", "ALTER TABLE schema_revisions COMMENT='ocservia-schema:0000000000000000000000000000000000000000000000000000000000000000'"} {
		t.Run(tamper, func(t *testing.T) {
			// Restore only task-owned test receipts before the next admission case.
			schema, err := artifactSources.ReadFile("mysql/schema.sql")
			if err != nil {
				t.Fatal(err)
			}
			a, err := loadMySQLArtifacts()
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{"DELETE FROM schema_revisions", "ALTER TABLE schema_revisions COMMENT='" + artifactCommentPrefix + digest(schema) + "'"} {
				if _, err = b.Exec(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = b.Exec(ctx, "INSERT INTO schema_revisions(epoch,revision,checksum,state,step,verified_at) VALUES(2,1,?,'verified',?,CURRENT_TIMESTAMP(6))", digest(schema), len(a.schema.Baseline.Steps)); err != nil {
				t.Fatal(err)
			}
			if _, err = b.Exec(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			before := mysqlArtifactDigest(t, b)
			if err = b.Migrate(ctx, ""); err == nil {
				t.Fatal("unsupported state adopted")
			}
			if before != mysqlArtifactDigest(t, b) {
				t.Fatal("refusal mutated schema or journal")
			}
		})
	}
}
func TestMySQLArtifactInterruptedUpgrade(t *testing.T) {
	ctx := context.Background()
	b, _, _ := migrateFixture(t)
	schema, upgrade := futureMySQLArtifacts(t)
	useMySQLArtifacts(t, schema, upgrade)
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	r := a.upgrade.Revisions[len(a.upgrade.Revisions)-1]
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = startMySQLRevision(ctx, conn, a.schema.Epoch, 2, fmt.Sprintf("%x", r.Checksum)); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, string(r.Steps[0].SQL)); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	before := mysqlArtifactDigest(t, b)
	for _, repair := range []string{"", "wrong"} {
		if err = b.Migrate(ctx, repair); err == nil {
			t.Fatal("running state silently resumed")
		}
		if mysqlArtifactDigest(t, b) != before {
			t.Fatal("refusal mutated journal")
		}
	}
	if err = b.Migrate(ctx, fmt.Sprintf("%x", r.Checksum)); err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	var value int
	if err = b.QueryRow(ctx, "SELECT value FROM artifact_probe").Scan(&value); err != nil || value != 2 {
		t.Fatal("non-reentrant data replayed", value, err)
	}
}
func TestMySQLArtifactDataRollback(t *testing.T) {
	ctx := context.Background()
	b, _, _ := migrateFixture(t)
	schema, upgrade := futureMySQLArtifacts(t)
	upgrade = []byte(strings.Replace(string(upgrade), "FROM artifact_probe WHERE id=1 AND value=2", "FROM artifact_probe WHERE id=1 AND value=999", 1))
	schema, upgrade = pinMySQLFuture(t, schema, upgrade)
	useMySQLArtifacts(t, schema, upgrade)
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrSchema) {
		t.Fatal("invalid data verification accepted", err)
	}
	var progress, value int
	if err := b.QueryRow(ctx, "SELECT step FROM schema_revisions WHERE revision=2").Scan(&progress); err != nil || progress != 2 {
		t.Fatal(progress, err)
	}
	if err := b.QueryRow(ctx, "SELECT value FROM artifact_probe").Scan(&value); err != nil || value != 1 {
		t.Fatal("failed data step committed", value, err)
	}
	before := mysqlArtifactDigest(t, b)
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal(err)
	}
	if before != mysqlArtifactDigest(t, b) {
		t.Fatal("dirty refusal mutated")
	}
}
func TestMySQLArtifactHistoryMutation(t *testing.T) {
	b, _, _ := migrateFixture(t)
	schema, upgrade := futureMySQLArtifacts(t)
	upgrade = []byte(strings.Replace(string(upgrade), "UPDATE artifact_probe SET", "UPDATE  artifact_probe SET", 1))
	useMySQLArtifacts(t, schema, upgrade)
	before := mysqlArtifactDigest(t, b)
	if err := b.Migrate(context.Background(), ""); !errors.Is(err, ErrChecksum) {
		t.Fatal(err)
	}
	if before != mysqlArtifactDigest(t, b) {
		t.Fatal("history mutation wrote database")
	}
}
func TestMySQLArtifactCrashBoundaries(t *testing.T) {
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", a.schema.Checksum)
	publish := "UPDATE schema_revisions SET checksum=?,step=?,state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE epoch=? AND revision=? AND state='running'"
	for _, test := range []struct {
		name, command     string
		forward, verified bool
	}{
		{"running-before-statement", string(a.schema.Baseline.Steps[0].SQL), false, false},
		{"ddl-before-progress", string(a.schema.Baseline.Steps[0].SQL), true, false},
		{"progress-after-commit", "UPDATE schema_revisions SET step=? WHERE epoch=? AND revision=? AND state='running' AND step=?", true, false},
		{"before-verified", publish, false, false},
		{"after-verified", publish, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _, o := fixture(t)
			cfg, err := driver.ParseDSN(o.DSN)
			if err != nil {
				t.Fatal(err)
			}
			proxy := newFinalizeProxy(t, cfg.Addr, test.command, test.forward)
			cfg.Addr = proxy.listener.Addr().String()
			child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=90s")
			child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_CRASH_LEGACY=no", "PR02_DSN="+cfg.FormatDSN())
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			defer child.Process.Kill()
			select {
			case <-proxy.hit:
			case <-time.After(35 * time.Second):
				child.Process.Kill()
				child.Wait()
				t.Fatal("child did not reach boundary")
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			child.Wait()
			proxy.close()
			if test.verified {
				if err = b.Migrate(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
			} else {
				before := mysqlArtifactDigest(t, b)
				if err = b.Migrate(context.Background(), ""); !errors.Is(err, ErrDirty) {
					t.Fatal(err)
				}
				if err = b.Migrate(context.Background(), "wrong"); !errors.Is(err, ErrChecksum) {
					t.Fatal(err)
				}
				if before != mysqlArtifactDigest(t, b) {
					t.Fatal("rejected repair mutated")
				}
				if err = b.Migrate(context.Background(), sum); err != nil {
					t.Fatal("exact repair", err)
				}
			}
			if err = b.ValidateSchema(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLArtifactCatalog(t *testing.T) {
	schema, err := artifactSources.ReadFile("mysql/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	upgrade, err := artifactSources.ReadFile("mysql/upgrade.sql")
	if err != nil {
		t.Fatal(err)
	}
	useMySQLArtifacts(t, schema, upgrade)
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	// An unknown definition must never normalize to absence or a pinned object.
	if got := a.fingerprint("foreign table definition"); got != digest([]byte("foreign table definition")) {
		t.Fatal("unknown fingerprint adopted", got)
	}
	if a.schema.Epoch != 2 || a.schema.Baseline.Number != 1 || a.upgrade.Previous == nil || a.upgrade.Previous.Ref != previousCheckpointRef || a.upgrade.Previous.Receipt.Checksum != previousCheckpointChecksum {
		t.Fatal("published checkpoint identity changed")
	}
}

func TestMySQLArtifactForeignRepair(t *testing.T) {
	ctx := context.Background()
	schema, upgrade := futureMySQLArtifacts(t)
	original := artifactSources
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			artifactSources = original
			b, _, _ := migrateFixture(t)
			useMySQLArtifacts(t, schema, upgrade)
			a, err := loadMySQLArtifacts()
			if err != nil {
				t.Fatal(err)
			}
			rev := a.upgrade.Revisions[len(a.upgrade.Revisions)-1]
			conn, err := b.pool.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = startMySQLRevision(ctx, conn, a.schema.Epoch, 2, fmt.Sprintf("%x", rev.Checksum)); err != nil {
				t.Fatal(err)
			}
			if partial {
				if _, err = conn.ExecContext(ctx, "CREATE TABLE artifact_probe(id INT PRIMARY KEY,value INT NOT NULL,foreign_column INT) ENGINE=InnoDB"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = conn.ExecContext(ctx, "CREATE TABLE foreign_probe(id INT) ENGINE=InnoDB"); err != nil {
					t.Fatal(err)
				}
			}
			conn.Close()
			before := mysqlArtifactDigest(t, b)
			if err = b.Migrate(ctx, fmt.Sprintf("%x", rev.Checksum)); !errors.Is(err, ErrSchema) {
				t.Fatal("foreign state adopted", err)
			}
			if mysqlArtifactDigest(t, b) != before {
				t.Fatal("foreign repair mutated state")
			}
		})
	}
}
