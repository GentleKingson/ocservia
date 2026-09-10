package mysql

import (
	"bytes"
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

func TestRealAuditMigrationGuardAfterCrash(t *testing.T) {
	b, options := versionTwoFixture(t, false)
	ctx := context.Background()
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	if latestRevisionVersion < 4 {
		t.Fatal("audit workflow requires revision four")
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	err = b.migrateChainOn(ctx, conn, chain[:2], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	workspace, id := uuid.New(), uuid.New()
	if _, err = b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'audit migration',?,NOW(6),NOW(6))`, UUIDBytes(workspace), workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,request_id,result,event_hash,before_summary) VALUES(?,?,'1000-01-01','controller','test','migration','workspace','test','intent',?,?)`, UUIDBytes(id), UUIDBytes(workspace), bytes.Repeat([]byte{1}, 32), `{"exact":1}`); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = b.QueryRow(ctx, `SELECT DATABASE()`).Scan(&schema); err != nil || !identifier.MatchString(schema) {
		t.Fatal("invalid test schema", err)
	}
	if _, err = b.Exec(ctx, "GRANT SELECT,UPDATE(event_hash) ON `"+schema+"`.`audit_events` TO 'ocservia_app'@'%'"); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	var target string
	_, parent, err := baselineFor(b.engine, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range chain[2].Parents[parent].Steps {
		if step.Name == "audit_events_occurred_at_column" {
			target = step.SQL
		}
	}
	if target == "" {
		t.Fatal("audit migration step missing")
	}
	proxy := newFinalizeProxy(t, cfg.Addr, target, false)
	cfg.Addr = proxy.listener.Addr().String()
	child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=120s")
	child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_DSN="+cfg.FormatDSN())
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	select {
	case <-proxy.hit:
	case <-time.After(90 * time.Second):
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal("audit guard not reached")
	}
	cfg, err = driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	runtimeOptions := options
	runtimeOptions.DSN = cfg.FormatDSN()
	runtime, err := Open(ctx, runtimeOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	deny := func() {
		t.Helper()
		for _, query := range []string{`UPDATE audit_events SET event_hash=event_hash WHERE id=?`, `UPDATE audit_events SET event_hash=REPEAT(0x00,32) WHERE id=?`} {
			if _, err = runtime.Exec(ctx, query, UUIDBytes(id)); err == nil {
				t.Fatal("audit mutation while immutable trigger absent")
			}
		}
	}
	deny()
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	proxy.close()
	deny()
	if err = b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal("unapproved crash recovery", err)
	}
	sum, err := ManifestChecksum(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, sum); err != nil {
		t.Fatal(err)
	}
	deny()
	var at value.Timestamp
	var hash []byte
	var summary value.JSONB
	if err = b.QueryRow(ctx, `SELECT occurred_at,event_hash,before_summary FROM audit_events WHERE id=?`, UUIDBytes(id)).Scan(&at, &hash, &summary); err != nil {
		t.Fatal(err)
	}
	want, _ := value.FromTime(time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC))
	if at != want || !bytes.Equal(hash, bytes.Repeat([]byte{1}, 32)) || string(summary.Bytes()) != `{"exact":1}` {
		t.Fatal("audit migration changed logical history", at, summary)
	}
}
