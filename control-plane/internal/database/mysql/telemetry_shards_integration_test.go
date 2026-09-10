package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryhistory"
	"github.com/google/uuid"
)

// This tests the physical shard lifecycle, not Controller workflow acceptance.
func TestRealTelemetryShardLifecycle(t *testing.T) {
	b, _, options := fixture(t)
	ctx := context.Background()
	collation := "utf8mb4_0900_bin"
	if b.engine == MariaDB {
		collation = "utf8mb4_nopad_bin"
	}
	template, err := TelemetryShardTemplateDDL(collation)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`CREATE TABLE nodes(id VARBINARY(16) PRIMARY KEY) ENGINE=InnoDB`, `CREATE TABLE telemetry_ingest_batches(batch_id VARBINARY(16) PRIMARY KEY) ENGINE=InnoDB`, `CREATE TABLE business_locks(lock_key VARBINARY(128) PRIMARY KEY) ENGINE=InnoDB`, `INSERT INTO business_locks VALUES('telemetry-shard-catalog')`, TelemetryShardCatalogDDL, template, TelemetryRetireShardsDDL, telemetryShardDDL("telemetry_samples", value.MinTimestamp, value.EndTimestamp, collation)} {
		if _, err = b.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 9, 9, 12, 34, 56, 123456000, time.UTC)
	node, batch := uuid.New(), uuid.New()
	if _, err = b.Exec(ctx, `INSERT INTO nodes(id) VALUES(?)`, UUIDBytes(node)); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id) VALUES(?)`, UUIDBytes(batch)); err != nil {
		t.Fatal(err)
	}
	micros, _ := telemetryMicros(at)
	if _, err = b.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,'cpu_usage_ratio',0.5)`, UUIDBytes(node), UUIDBytes(batch), micros); err != nil {
		t.Fatal(err)
	}
	reader, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	readerStore, _ := telemetryhistory.FromTransaction(reader)
	legacyPoints, err := readerStore.History(ctx, node, "cpu_usage_ratio", "raw", fixtureTimestamp(t, at.Add(-time.Second)))
	if err != nil || len(legacyPoints) != 1 {
		t.Fatal(legacyPoints, err)
	}
	blocked, cancelProvision := context.WithTimeout(ctx, 200*time.Millisecond)
	err = b.ProvisionTelemetryMonth(blocked, at)
	cancelProvision()
	if err == nil {
		t.Fatal("legacy activation bypassed a reader's catalog snapshot")
	}
	var remaining int
	if err = b.QueryRow(ctx, `SELECT count(*) FROM telemetry_samples`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatal("cancelled activation lost legacy data", remaining, err)
	}
	if err = reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.ProvisionTelemetryMonth(ctx, at); err != nil {
		for _, table := range []string{"telemetry_samples_template", "telemetry_samples_m_202609"} {
			var name, ddl string
			_ = b.QueryRow(ctx, "SHOW CREATE TABLE "+table).Scan(&name, &ddl)
			t.Log(ddl)
		}
		t.Fatal(err)
	}
	if err = b.ProvisionTelemetryMonth(ctx, at); err != nil {
		t.Fatal("repeat provision", err)
	}
	var legacy int
	if err = b.QueryRow(ctx, `SELECT count(*) FROM telemetry_samples`).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatal("legacy sample did not move atomically", legacy, err)
	}
	name, _, _, _ := telemetryMonth(at)
	if err = b.GrantTelemetryTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	options.DSN = strings.Replace(options.DSN, "ocservia_owner:pr02-owner-test-only@", "ocservia_app:pr02-runtime-test-only@", 1)
	runtime, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for _, query := range []string{`UPDATE telemetry_sample_shards SET state='retired'`, `UPDATE ` + name + ` SET value=0`, `DELETE FROM ` + name, `DROP TABLE ` + name} {
		if _, err = runtime.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
			t.Fatal("runtime shard privilege broadened", query, err)
		}
	}
	if _, err = runtime.Exec(ctx, `CALL telemetry_retire_shards(?)`, int64(9223372036854775807)); err == nil {
		t.Fatal("unbounded retention accepted")
	}
	samples := []telemetryhistory.Sample{{SampledAt: at, Metric: "cpu_usage_ratio", Value: 0.5}}
	if err = database.Within(ctx, runtime, database.ReadCommitted, func(tx database.Tx) error {
		s, _ := telemetryhistory.FromTransaction(tx)
		if err := s.Insert(ctx, node, batch, samples); err != nil {
			return err
		}
		return s.Insert(ctx, node, batch, samples)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `DELETE FROM nodes WHERE id=?`, UUIDBytes(node)); !errors.Is(err, database.ErrForeignKey) {
		t.Fatal("node FK restriction lost", err)
	}
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	history, _ := telemetryhistory.FromTransaction(tx)
	points, err := history.History(ctx, node, "cpu_usage_ratio", "raw", fixtureTimestamp(t, at.Add(-time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].At != fixtureTimestamp(t, at) || points[0].Average != 0.5 {
		t.Fatalf("history lost precision or replay uniqueness: %+v", points)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = b.Exec(short, `UPDATE telemetry_sample_shards SET state='retired' WHERE table_name=?`, name)
	cancel()
	if err == nil {
		t.Fatal("retirement did not wait for active reader")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `DELETE FROM telemetry_ingest_batches WHERE batch_id=?`, UUIDBytes(batch)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = b.QueryRow(ctx, `SELECT count(*) FROM `+name).Scan(&count); err != nil || count != 0 {
		t.Fatal("batch FK cascade lost", count, err)
	}
	if _, err = b.Exec(ctx, `UPDATE telemetry_sample_shards SET state='retired' WHERE table_name=?`, name); err != nil {
		t.Fatal(err)
	}
	if err = b.CollectRetiredTelemetryShards(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.CollectRetiredTelemetryShards(ctx); err != nil {
		t.Fatal("repeat GC", err)
	}
	if err = b.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, name).Scan(&count); err != nil || count != 0 {
		t.Fatal("physical table not dropped", count, err)
	}
	if err = b.ProvisionTelemetryMonth(ctx, at); !errors.Is(err, ErrSchema) {
		t.Fatal("retired month resurrected", err)
	}
}
