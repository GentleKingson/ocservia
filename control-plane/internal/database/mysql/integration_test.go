package mysql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	dsn := os.Getenv("PR02_DSN")
	if dsn == "" {
		t.Skip("real database test: PR02_DSN required")
	}
	return Options{Engine: Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
}
func fixture(t *testing.T) (*Backend, *Backend, Options) {
	t.Helper()
	ctx := context.Background()
	o := testOptions(t)
	admin, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "pr02_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error(err)
		}
	})
	if _, err = admin.Exec(ctx, "GRANT ALL ON `"+name+"`.* TO 'ocservia_owner'@'%' WITH GRANT OPTION"); err != nil {
		t.Fatal(err)
	}
	c, err := driver.ParseDSN(o.DSN)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	c.DBName = name
	c.User = "ocservia_owner"
	c.Passwd = "pr02-owner-test-only"
	o.DSN = c.FormatDSN()
	b, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, admin, o
}
func migrateFixture(t *testing.T) (*Backend, *Backend, Options) {
	t.Helper()
	b, a, o := fixture(t)
	if err := b.Migrate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	return b, a, o
}

func TestRealInitializationAndHistory(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateSchema(ctx, 33); !errors.Is(err, ErrSchema) {
		t.Fatal("incorrect compatibility accepted")
	}
	if _, err := b.Exec(ctx, "UPDATE backend_migrations SET manifest_checksum=REPEAT('0',64)"); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrChecksum) {
		t.Fatal("checksum conflict accepted")
	}
}

func TestRealTLS(t *testing.T) {
	o := testOptions(t)
	ca := os.Getenv("PR02_TLS_CA_FILE")
	if ca == "" {
		t.Fatal("real TLS test requires PR02_TLS_CA_FILE")
	}
	o.DSN = strings.Replace(o.DSN, "tls=false", "tls=true", 1)
	o.CAFile = ca
	b, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var name, cipher string
	if err = b.QueryRow(context.Background(), "SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher); err != nil || cipher == "" {
		t.Fatal("TLS session not established")
	}
	o.CAFile = ""
	if untrusted, err := Open(context.Background(), o); err == nil {
		untrusted.Close()
		t.Fatal("untrusted certificate accepted")
	}
}
func TestRealConcurrentMigration(t *testing.T) {
	b, _, _ := fixture(t)
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- b.Migrate(context.Background(), "") }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
func TestRealSchemaDriftRepairRefused(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	if _, err := b.Exec(ctx, "ALTER TABLE workspaces ADD COLUMN unexpected INT"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, "UPDATE backend_schema_revisions SET state='running' WHERE version=?", latestRevisionVersion); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal("dirty history accepted", err)
	}
	sum, _ := ManifestChecksum(b.engine)
	if err := b.Migrate(ctx, sum); !errors.Is(err, ErrSchema) {
		t.Fatal("repair adopted foreign schema", err)
	}
}

func TestCrashChild(t *testing.T) {
	if os.Getenv("PR02_CRASH_CHILD") != "yes" {
		t.Skip("subprocess only")
	}
	b, err := Open(context.Background(), testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.Migrate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}
func TestRealCrashAndRepair(t *testing.T) {
	b, admin, o := fixture(t)
	ctx := context.Background()
	for _, ddl := range []string{metadataDDL, stepsDDL, "CREATE TRIGGER pause_verification BEFORE UPDATE ON backend_migration_steps FOR EACH ROW SET @pause= SLEEP(60)"} {
		if _, err := b.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=90s")
	child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_DSN="+o.DSN)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	m, _, _ := loadManifest(b.engine)
	deadline := time.Now().Add(15 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		var count int
		err := b.QueryRow(ctx, "SELECT count(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", m.Steps[0].Name).Scan(&count)
		if err == nil && count == 1 {
			observed = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !observed {
		t.Fatal("child did not reach committed DDL")
	}
	var name string
	if err := b.QueryRow(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	var connection int64
	if err := b.QueryRow(ctx, "SELECT IS_USED_LOCK(?)", "ocservia:"+digest([]byte(name))[:48]).Scan(&connection); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	// Ensure the killed client's server-side sleep/session has also terminated.
	if _, err := admin.pool.ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", connection)); err != nil {
		var serverError *driver.MySQLError
		if !errors.As(err, &serverError) || serverError.Number != 1094 {
			t.Fatal(safeError(err))
		}
	}
	if _, err := b.Exec(ctx, "DROP TRIGGER pause_verification"); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatalf("crash not dirty: %v", err)
	}
	if err := b.Migrate(ctx, "wrong"); !errors.Is(err, ErrChecksum) {
		t.Fatal("invalid repair token")
	}
	sum, _ := ManifestChecksum(b.engine)
	if err := b.Migrate(ctx, sum); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}

func TestRealCancellationAndUnlockFailure(t *testing.T) {
	b, _, _ := fixture(t)
	ctx := context.Background()
	b.pool.SetMaxOpenConns(1)
	b.pool.SetMaxIdleConns(1)
	conn, name, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	var previous int
	if err = conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", name); err != nil {
		t.Fatal(err)
	}
	if err = releaseMigrationConnection(conn, name); !errors.Is(err, ErrUnlock) {
		t.Fatal("unconfirmed unlock accepted")
	}
	var next int
	if err = b.QueryRow(ctx, "SELECT CONNECTION_ID()").Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next == previous {
		t.Fatal("unlocked connection returned to pool instead of discarded")
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	var slept int
	err = b.QueryRow(cancelCtx, "SELECT SLEEP(10)").Scan(&slept)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err = b.QueryRow(ctx, "SELECT CONNECTION_ID()").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	if next == previous {
		t.Fatal("cancelled connection reused")
	}
}

func TestRealSessionAndLocks(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	var zone, isolation, collation string
	if err := b.QueryRow(ctx, "SELECT @@session.time_zone,@@session.transaction_isolation,@@session.collation_connection").Scan(&zone, &isolation, &collation); err != nil {
		t.Fatal(err)
	}
	if zone != "+00:00" || isolation != "READ-COMMITTED" || !strings.Contains(collation, "bin") {
		t.Fatal("session policy mismatch")
	}
	var timestamp time.Time
	input := time.Date(2026, 9, 9, 1, 2, 3, 123456789, time.UTC)
	if err := b.QueryRow(ctx, "SELECT CAST(? AS DATETIME(6))", input).Scan(&timestamp); err != nil {
		t.Fatal(err)
	}
	if !timestamp.Equal(input.Truncate(time.Microsecond)) {
		t.Fatal("time precision mismatch")
	}
	if err := b.QueryRow(ctx, "SELECT CAST('2026-09-09 01:02:03.123456789' AS DATETIME(6))").Scan(&timestamp); err != nil || !timestamp.Equal(input.Truncate(time.Microsecond)) {
		t.Fatal("server fractional precision policy mismatch")
	}
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = LockTransaction(ctx, tx, "global:734821032"); err != nil {
		t.Fatal(err)
	}
	tx2, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	if err = LockTransaction(ctx, tx2, "global:734821033"); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err = LockTransaction(wait, tx2, "global:734821032"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("same-key transactions did not serialize")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error { return LockTransaction(ctx, tx, "global:734821032") }); err != nil {
		t.Fatal(err)
	}
}

func TestRealPrivileges(t *testing.T) {
	b, _, o := migrateFixture(t)
	ctx := context.Background()
	if err := b.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := driver.ParseDSN(o.DSN)
	c.User = "ocservia_app"
	c.Passwd = "pr02-runtime-test-only"
	o.DSN = c.FormatDSN()
	runtime, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	assertRuntimeDiagnostics(t, b, runtime)
	t.Run("telemetry-privilege-upgrade", func(t *testing.T) {
		if err := b.PrepareControllerTelemetry(ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"telemetry_rollups_5m", "telemetry_rollups_1h"} {
			if _, err := b.Exec(ctx, "GRANT DELETE ON `"+c.DBName+"`.`"+table+"` TO 'ocservia_app'@'%'"); err != nil {
				t.Fatal(err)
			}
		}
		if err := runtime.ValidateTelemetryRuntime(ctx); err == nil {
			t.Fatal("legacy unrestricted DELETE accepted")
		}
		if err := b.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		if err := runtime.ValidateTelemetryRuntime(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(ctx, "REVOKE EXECUTE ON PROCEDURE `"+c.DBName+"`.telemetry_prune_rollups FROM 'ocservia_app'@'%'"); err != nil {
			t.Fatal(err)
		}
		if err := runtime.ValidateTelemetryRuntime(ctx); err == nil {
			t.Fatal("missing cleanup EXECUTE accepted")
		}
		if err := b.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(ctx, "GRANT DELETE ON `"+c.DBName+"`.* TO 'ocservia_app'@'%'"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := b.Exec(ctx, "REVOKE DELETE ON `"+c.DBName+"`.* FROM 'ocservia_app'@'%'"); err != nil {
				t.Error(err)
			}
		}()
		if err := runtime.ValidateTelemetryRuntime(ctx); err == nil {
			t.Fatal("database-wide DELETE accepted")
		}
	})
	for _, table := range []string{"backend_schema_revisions", "backend_schema_revision_steps"} {
		var count int
		if err := runtime.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("runtime cannot read revision history: %v", err)
		}
		for _, query := range []string{"UPDATE " + table + " SET state='verified'", "DELETE FROM " + table, "INSERT INTO " + table + "(version) VALUES(999)"} {
			if _, err := runtime.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
				t.Fatalf("runtime can mutate revision history: %v", err)
			}
		}
	}
	for _, query := range []string{"SELECT * FROM backend_migrations", "UPDATE local_auth_bootstrap SET completion_pending=completion_pending WHERE singleton=0", "UPDATE transport_events SET transport_cursor_valid=transport_cursor_valid WHERE 1=0"} {
		if _, err := runtime.Exec(ctx, query); err != nil {
			t.Fatalf("allowed operation denied: %v", err)
		}
	}
	for _, query := range []string{"UPDATE backend_migrations SET dirty=FALSE", "DELETE FROM backend_migration_steps", "CREATE TABLE forbidden(id INT)", "ALTER TABLE workspaces ADD COLUMN forbidden INT", "TRUNCATE TABLE audit_events", "UPDATE audit_events SET reason='forbidden'", "DELETE FROM audit_checkpoints", "UPDATE local_auth_bootstrap SET identity_id=NULL", "UPDATE transport_events SET payload=0x00", "GRANT SELECT ON workspaces TO 'ocservia_maintenance'@'%'"} {
		if _, err := runtime.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
			t.Fatalf("forbidden operation not denied: %s: %v", strings.Fields(query)[0], err)
		}
	}
	c.User = "ocservia_maintenance"
	c.Passwd = "pr02-maintenance-test-only"
	o.DSN = c.FormatDSN()
	maintenance, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	if _, err = maintenance.Exec(ctx, "DELETE FROM telemetry_samples WHERE 1=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = maintenance.Exec(ctx, "DELETE FROM audit_events"); !errors.Is(err, database.ErrPermission) {
		t.Fatal("maintenance can rewrite audit")
	}
	if _, err = maintenance.Exec(ctx, "UPDATE backend_migrations SET dirty=FALSE"); !errors.Is(err, database.ErrPermission) {
		t.Fatal("maintenance can repair migration metadata")
	}
}

func TestRealValueConstraints(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	workspace := UUIDBytes(uuid.New())
	for _, slug := range []string{"Case", "case", "case "} {
		id := UUIDBytes(uuid.New())
		if slug == "Case" {
			id = workspace
		}
		if _, err := b.Exec(ctx, "INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))", id, "test", slug); err != nil {
			t.Fatal("case/trailing-space uniqueness mismatch", err)
		}
	}
	if _, err := b.Exec(ctx, "INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,'case',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))", UUIDBytes(uuid.New()), "test"); !errors.Is(err, database.ErrUnique) {
		t.Fatal("exact duplicate accepted")
	}
	if _, err := b.Exec(ctx, "UPDATE workspaces SET version=0 WHERE id=?", workspace); !errors.Is(err, database.ErrConstraint) {
		t.Fatal("CHECK not enforced")
	}
	if _, err := b.Exec(ctx, "INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'n','active',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))", UUIDBytes(uuid.New()), UUIDBytes(uuid.New())); !errors.Is(err, database.ErrForeignKey) {
		t.Fatal("foreign key not enforced")
	}
	identity := UUIDBytes(uuid.New())
	if _, err := b.Exec(ctx, "INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,'local','person',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))", identity); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_, err := b.Exec(ctx, "INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'Viewer','workspace',CURRENT_TIMESTAMP(6))", UUIDBytes(uuid.New()), identity, workspace)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && !errors.Is(err, database.ErrUnique) {
			t.Fatal("NULLS NOT DISTINCT not enforced")
		}
	}
	// NULL idempotency keys remain distinct; matching non-NULL keys do not.
	for i := 0; i < 2; i++ {
		if _, err := b.Exec(ctx, "INSERT INTO operations(id,workspace_id,state,request_id,created_at,updated_at) VALUES(?,?,'draft','test',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))", UUIDBytes(uuid.New()), workspace); err != nil {
			t.Fatal(err)
		}
	}
}
