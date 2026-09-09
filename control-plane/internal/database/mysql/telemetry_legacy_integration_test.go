package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

// The owner API verifies the complete published ledger before touching data;
// these tests therefore require the real latest manifest, not candidate DDL.
func legacyTelemetryFixture(t *testing.T) (*Backend, Options, uuid.UUID, uuid.UUID) {
	t.Helper()
	b, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace, node, batch := uuid.New(), uuid.New(), uuid.New()
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'legacy-history',?,CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(workspace), workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'legacy-history','active',CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(node), UUIDBytes(workspace)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES(?,?,1,'raw_history',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)),1)`, UUIDBytes(batch), UUIDBytes(node)); err != nil {
		t.Fatal(err)
	}
	return b, options, node, batch
}

func legacyInsert(ctx context.Context, b *Backend, node, batch uuid.UUID, at int64, n float64) error {
	_, err := b.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,'cpu_usage_ratio',?)`, UUIDBytes(node), UUIDBytes(batch), at, n)
	return err
}

func TestRealTelemetryAutomaticLegacyMigration(t *testing.T) {
	b, options, node, batch := legacyTelemetryFixture(t)
	ctx := context.Background()
	finite := []int64{value.MinTimestamp, value.EndTimestamp - 1}
	for _, at := range []time.Time{time.Date(0, 1, 1, 0, 0, 0, 1000, time.UTC), time.Date(1000, 1, 1, 0, 0, 0, 123456000, time.UTC), time.Unix(-1, 999999000).UTC(), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 1000, time.UTC)} {
		v, err := value.FromTime(at)
		if err != nil {
			t.Fatal(err)
		}
		finite = append(finite, v.Micros)
	}
	for _, at := range append(append([]int64{}, finite...), value.NegativeInfinity, value.PositiveInfinity) {
		if err := legacyInsert(ctx, b, node, batch, at, 0.5); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.ValidateTelemetryHistoryReady(ctx); !errors.Is(err, ErrTelemetryHistoryPending) {
		t.Fatal("pending history accepted", err)
	}
	cutoff, _ := telemetryMicros(time.Now().UTC().Add(-14 * 24 * time.Hour))
	if _, err := b.Exec(ctx, `CALL telemetry_retire_shards(?)`, cutoff); err == nil {
		t.Fatal("retention bypassed pending history migration")
	}
	if err := b.MigrateTelemetryHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateTelemetryHistoryReady(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := b.QueryRow(ctx, `SELECT count(*) FROM telemetry_samples`).Scan(&count); err != nil || count != 2 {
		t.Fatal("default infinity values changed", count, err)
	}
	for _, micros := range finite {
		at, err := (value.Timestamp{Micros: micros, Valid: true}).Time()
		if err != nil {
			t.Fatal(err)
		}
		name, _, _, err := telemetryMonth(at)
		if err != nil {
			t.Fatal(err)
		}
		var stored int64
		var n float64
		if err = b.QueryRow(ctx, `SELECT sampled_at,value FROM `+name+` WHERE sampled_at=?`, micros).Scan(&stored, &n); err != nil || stored != micros || n != 0.5 {
			t.Fatal("finite history was not preserved", micros, stored, n, err)
		}
	}
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = validateTelemetryShards(ctx, conn)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	var completed string
	if err = b.QueryRow(ctx, `SELECT CAST(completed_at AS CHAR) FROM telemetry_legacy_migration WHERE singleton=1`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if err = b.MigrateTelemetryHistory(ctx); err != nil {
		t.Fatal("repeat", err)
	}
	var repeated string
	if err = b.QueryRow(ctx, `SELECT CAST(completed_at AS CHAR) FROM telemetry_legacy_migration WHERE singleton=1`).Scan(&repeated); err != nil || completed != repeated {
		t.Fatal("completion receipt rewritten", completed, repeated, err)
	}
	if err = b.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	options.DSN = strings.Replace(options.DSN, "ocservia_owner:pr02-owner-test-only@", "ocservia_app:pr02-runtime-test-only@", 1)
	runtime, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = runtime.ValidateTelemetryHistoryReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err = runtime.MigrateTelemetryHistory(ctx); !errors.Is(err, database.ErrPermission) {
		t.Fatal("runtime executed owner completion", err)
	}
	if _, err = runtime.Exec(ctx, `UPDATE telemetry_legacy_migration SET state='running',completed_at=NULL`); !errors.Is(err, database.ErrPermission) {
		t.Fatal("runtime rewrote completion", err)
	}
	if err = legacyInsert(ctx, runtime, node, batch, 0, 1); err == nil {
		t.Fatal("finite legacy write accepted after completion")
	}
	if _, err = b.Exec(ctx, `UPDATE telemetry_samples SET sampled_at=0 WHERE sampled_at=?`, value.NegativeInfinity); err == nil {
		t.Fatal("finite legacy update accepted after completion")
	}
	if _, err = b.Exec(ctx, `DELETE FROM telemetry_samples WHERE sampled_at=?`, value.PositiveInfinity); err != nil {
		t.Fatal(err)
	}
	if err = legacyInsert(ctx, runtime, node, batch, value.PositiveInfinity, 0.5); err != nil {
		t.Fatal("default infinity rejected", err)
	}
	if _, err = b.Exec(ctx, `DELETE FROM nodes WHERE id=?`, UUIDBytes(node)); !errors.Is(err, database.ErrForeignKey) {
		t.Fatal("monthly node restriction lost", err)
	}
	if _, err = b.Exec(ctx, `DELETE FROM telemetry_ingest_batches WHERE batch_id=?`, UUIDBytes(batch)); err != nil {
		t.Fatal(err)
	}
	for _, micros := range finite {
		at, _ := (value.Timestamp{Micros: micros, Valid: true}).Time()
		name, _, _, _ := telemetryMonth(at)
		if err = b.QueryRow(ctx, `SELECT count(*) FROM `+name).Scan(&count); err != nil || count != 0 {
			t.Fatal("monthly batch cascade lost", count, err)
		}
	}
}

func TestRealTelemetryLegacyConflictRecovery(t *testing.T) {
	b, _, node, batch := legacyTelemetryFixture(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	micros, _ := telemetryMicros(at)
	if err := legacyInsert(ctx, b, node, batch, micros, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.ProvisionTelemetryMonth(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := legacyInsert(ctx, b, node, batch, micros, 2); err != nil {
		t.Fatal(err)
	}
	if err := b.MigrateTelemetryHistory(ctx); !errors.Is(err, ErrSchema) {
		t.Fatal("conflicting target overwritten", err)
	}
	var n float64
	if err := b.QueryRow(ctx, `SELECT value FROM telemetry_samples WHERE sampled_at=?`, micros).Scan(&n); err != nil || n != 2 {
		t.Fatal("conflicting source lost", n, err)
	}
	name, _, _, _ := telemetryMonth(at)
	if err := b.QueryRow(ctx, `SELECT value FROM `+name+` WHERE sampled_at=?`, micros).Scan(&n); err != nil || n != 1 {
		t.Fatal("conflicting target lost", n, err)
	}
	if _, err := b.Exec(ctx, `UPDATE telemetry_samples SET value=1 WHERE sampled_at=?`, micros); err != nil {
		t.Fatal(err)
	}
	if err := b.MigrateTelemetryHistory(ctx); err != nil {
		t.Fatal("explicitly corrected source did not recover", err)
	}
}

func TestRealTelemetryLegacyCompletionRace(t *testing.T) {
	b, _, node, batch := legacyTelemetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := b.Exec(ctx, `UPDATE telemetry_legacy_migration SET state='running' WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	writer, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err = writer.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,0,'cpu_usage_ratio',1)`, UUIDBytes(node), UUIDBytes(batch)); err != nil {
		t.Fatal(err)
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			if err := releaseMigrationConnection(conn, lock); err != nil {
				t.Error(err)
			}
		}
	}()
	type result struct {
		complete bool
		err      error
	}
	done := make(chan result, 1)
	go func() { complete, err := finishTelemetryHistory(ctx, conn); done <- result{complete, err} }()
	select {
	case r := <-done:
		t.Fatal("completion bypassed pending legacy writer", r)
	case <-time.After(100 * time.Millisecond):
	}
	if err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	err = releaseMigrationConnection(conn, lock)
	released = true
	if err != nil {
		t.Fatal(err)
	}
	if r.complete || r.err != nil && !errors.Is(r.err, database.ErrSerialization) {
		t.Fatalf("completion missed concurrent committed sample: complete=%v err=%v", r.complete, r.err)
	}
	var state string
	if err = b.QueryRow(ctx, `SELECT state FROM telemetry_legacy_migration WHERE singleton=1`).Scan(&state); err != nil || state != "running" {
		t.Fatal("concurrent completion advanced receipt", state, err)
	}
	var count int
	if err = b.QueryRow(ctx, `SELECT count(*) FROM telemetry_samples WHERE sampled_at=0`).Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent completion lost finite source", count, err)
	}
	if err = b.MigrateTelemetryHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.ValidateTelemetryHistoryReady(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRealTelemetryLegacyBusyNodeResume(t *testing.T) {
	b, _, node, batch := legacyTelemetryFixture(t)
	ctx := context.Background()
	at := time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC)
	micros, _ := telemetryMicros(at)
	if err := b.ProvisionTelemetryMonth(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := legacyInsert(ctx, b, node, batch, micros, 1); err != nil {
		t.Fatal(err)
	}
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var id []byte
	if err = tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=? FOR UPDATE`, UUIDBytes(node)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = b.MigrateTelemetryHistory(bounded)
	cancel()
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("busy node did not fail immediately without retry", err)
	}
	var count int
	if err = b.QueryRow(ctx, `SELECT count(*) FROM telemetry_samples`).Scan(&count); err != nil || count != 1 {
		t.Fatal("busy migration lost source", count, err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.MigrateTelemetryHistory(ctx); err != nil {
		t.Fatal("planned month did not resume", err)
	}
}

func TestRealTelemetryLegacySchemaRefusal(t *testing.T) {
	b, _, node, batch := legacyTelemetryFixture(t)
	ctx := context.Background()
	if err := legacyInsert(ctx, b, node, batch, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `DROP TRIGGER telemetry_legacy_insert_guard`); err != nil {
		t.Fatal(err)
	}
	if err := b.MigrateTelemetryHistory(ctx); !errors.Is(err, ErrSchema) {
		t.Fatal("unverified source guard accepted", err)
	}
	var state string
	if err := b.QueryRow(ctx, `SELECT state FROM telemetry_legacy_migration WHERE singleton=1`).Scan(&state); err != nil || state != "pending" {
		t.Fatal("schema refusal rewrote receipt", state, err)
	}
	for _, step := range TelemetryLegacyMigrationSteps() {
		if step.Name == "telemetry_legacy_insert_guard" {
			if _, err := b.Exec(ctx, step.SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := b.MigrateTelemetryHistory(ctx); err != nil {
		t.Fatal("exact guard restoration did not recover", err)
	}
}

func TestRealTelemetryLegacyPartialProgress(t *testing.T) {
	b, _, node, batch := legacyTelemetryFixture(t)
	ctx := context.Background()
	early := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	late := early.AddDate(0, 1, 0)
	first, _ := telemetryMicros(early)
	second, _ := telemetryMicros(late)
	// Locking an existing source row avoids any reliance on DDL lock timing.
	if err := b.ProvisionTelemetryMonth(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := legacyInsert(ctx, b, node, batch, first, 1); err != nil {
		t.Fatal(err)
	}
	boundary, _ := telemetryMicros(time.Date(late.Year(), late.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err := legacyInsert(ctx, b, node, batch, boundary, 3); err != nil {
		t.Fatal(err)
	}
	if err := legacyInsert(ctx, b, node, batch, second, 2); err != nil {
		t.Fatal(err)
	}
	busy, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Rollback(ctx)
	var locked int64
	if err = busy.QueryRow(ctx, `SELECT sampled_at FROM telemetry_samples WHERE sampled_at=? FOR UPDATE`, second).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = b.MigrateTelemetryHistory(bounded)
	cancel()
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("busy month was retried or not detected", err)
	}
	name, _, _, _ := telemetryMonth(early)
	var count int
	if err = b.QueryRow(ctx, `SELECT count(*) FROM `+name+` WHERE sampled_at=?`, first).Scan(&count); err != nil || count != 1 {
		t.Fatal("completed prior month did not remain durable", count, err)
	}
	if err = b.QueryRow(ctx, `SELECT count(*) FROM telemetry_samples`).Scan(&count); err != nil || count != 2 {
		t.Fatal("partial migration lost or duplicated source", count, err)
	}
	if err = busy.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.MigrateTelemetryHistory(ctx); err != nil {
		t.Fatal("partial migration did not resume", err)
	}
	if err = b.ValidateTelemetryHistoryReady(ctx); err != nil {
		t.Fatal(err)
	}
}
