package mysql

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealMigrationProtectsVerifiedSourceCopy(t *testing.T) {
	b, o := versionTwoFixture(t, false)
	ctx := context.Background()
	workspace, node := UUIDBytes(uuid.New()), UUIDBytes(uuid.New())
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'guard',?,NOW(6),NOW(6))`, workspace, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'guard','active',NOW(6),NOW(6))`, node, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO telemetry_rollups_5m(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES(?,'cpu_usage_ratio','2000-01-01',1,1,1,1)`, node); err != nil {
		t.Fatal(err)
	}
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	_, parent, err := baselineFor(b.engine, "")
	if err != nil {
		t.Fatal(err)
	}
	var target revisionStep
	for _, s := range chain[1].Parents[parent].Steps {
		if s.Name == "telemetry_rollups_5m_time_activate" {
			target = s
		}
	}
	if target.CheckBeforeSQL == "" {
		t.Fatal("missing source-copy precondition")
	}
	c, err := driver.ParseDSN(o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFinalizeProxy(t, c.Addr, target.SQL, false)
	c.Addr = proxy.listener.Addr().String()
	child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=120s")
	child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_DSN="+c.FormatDSN())
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	select {
	case <-proxy.hit:
	case <-time.After(90 * time.Second):
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal("no source switch")
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	proxy.close()
	var state string
	if err = b.QueryRow(ctx, `SELECT state FROM backend_schema_revision_steps WHERE version=3 AND name='telemetry_rollups_5m_time_backfill'`).Scan(&state); err != nil || state != "verified" {
		t.Fatal(state, err)
	}
	if _, err = b.Exec(ctx, `UPDATE telemetry_rollups_5m SET bucket_at='2001-01-01' WHERE node_id=?`, node); err == nil {
		t.Fatal("unguarded writer changed source")
	}
	sum, _ := ManifestChecksum(b.engine)
	var guard revisionStep
	for _, s := range chain[1].Parents[parent].Steps {
		if s.Object == "migrate_telemetry_rollups_5m_update" && s.After != "" {
			guard = s
			break
		}
	}
	if guard.SQL == "" {
		t.Fatal("missing recorded writer guard")
	}
	if _, err = b.Exec(ctx, "DROP TRIGGER migrate_telemetry_rollups_5m_update"); err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, sum); !errors.Is(err, ErrSchema) {
		t.Fatal("repair ignored a missing writer guard", err)
	}
	if _, err = b.Exec(ctx, guard.SQL); err != nil {
		t.Fatal(err)
	}
	edit := func(query string) {
		t.Helper()
		conn, name, err := migrationConnection(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		_, changeErr := conn.ExecContext(ctx, query, node)
		unlockErr := releaseMigrationConnection(conn, name)
		if changeErr != nil || unlockErr != nil {
			t.Fatal(changeErr, unlockErr)
		}
	}
	// Even an explicit owner edit during downtime cannot silently invalidate
	// a previously verified copy when repair later consumes the source column.
	edit(`UPDATE telemetry_rollups_5m SET bucket_at='2001-01-01' WHERE node_id=?`)
	if err = b.Migrate(ctx, sum); !errors.Is(err, ErrSchema) {
		t.Fatal("stale verified copy adopted", err)
	}
	edit(`UPDATE telemetry_rollups_5m SET pr02_bucket_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',bucket_at) WHERE node_id=?`)
	if err = b.Migrate(ctx, sum); err != nil {
		t.Fatal(err)
	}
	var got value.Timestamp
	if err = b.QueryRow(ctx, `SELECT bucket_at FROM telemetry_rollups_5m WHERE node_id=?`, node).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want, _ := value.FromTime(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	if got != want {
		t.Fatal("source lost during recovery", got, want)
	}
}
