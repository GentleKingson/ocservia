package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRealTelemetryTimeMigration(t *testing.T) {
	b, _, _ := fixture(t)
	ctx := context.Background()
	for _, ddl := range []string{`CREATE TABLE nodes(id VARBINARY(16) PRIMARY KEY) ENGINE=InnoDB`, `CREATE TABLE telemetry_ingest_batches(batch_id VARBINARY(16) PRIMARY KEY) ENGINE=InnoDB`, `CREATE TABLE business_locks(lock_key VARBINARY(128) PRIMARY KEY) ENGINE=InnoDB`} {
		if _, err := b.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err := loadManifest(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Steps {
		if s.Name == "telemetry_samples" || s.Name == "telemetry_rollups_5m" || s.Name == "telemetry_rollups_1h" {
			if _, err = b.Exec(ctx, s.SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	node, batch := uuid.New(), uuid.New()
	if _, err = b.Exec(ctx, `INSERT INTO nodes VALUES(?)`, UUIDBytes(node)); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO telemetry_ingest_batches VALUES(?)`, UUIDBytes(batch)); err != nil {
		t.Fatal(err)
	}
	times := []time.Time{time.Date(1000, 1, 1, 0, 0, 0, 123456000, time.UTC), time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)}
	for _, at := range times {
		if _, err = b.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,'cpu_usage_ratio',0.5)`, UUIDBytes(node), UUIDBytes(batch), at); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"telemetry_rollups_5m", "telemetry_rollups_1h"} {
			if _, err = b.Exec(ctx, `INSERT INTO `+table+`(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES(?,'cpu_usage_ratio',?,1,0.5,0.5,0.5)`, UUIDBytes(node), at); err != nil {
				t.Fatal(err)
			}
		}
	}
	steps, err := TelemetryMigrationSteps(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if strings.HasSuffix(s.Name, "_time_backfill") {
			column := "pr02_bucket_at"
			order := "bucket_at"
			if s.Object == "telemetry_samples" {
				column = "pr02_sampled_at"
				order = "sampled_at"
			}
			if _, err = b.Exec(ctx, `UPDATE `+s.Object+` SET `+column+`=1 ORDER BY `+order+` LIMIT 1`); err != nil {
				t.Fatal(err)
			}
			if _, err = b.Exec(ctx, s.SQL); err != nil {
				t.Fatal(err)
			}
			var result string
			if err = b.QueryRow(ctx, s.VerifySQL).Scan(&result); err != nil || result != "invalid" {
				t.Fatal("conflicting shadow was silently overwritten", result, err)
			}
			// Only explicit owner correction may clear a conflicting shadow.
			if _, err = b.Exec(ctx, `UPDATE `+s.Object+` SET `+column+`=NULL WHERE `+column+`=1`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = b.Exec(ctx, s.SQL); err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		if s.VerifySQL != "" {
			var result string
			if err = b.QueryRow(ctx, s.VerifySQL).Scan(&result); err != nil || result != "valid" {
				t.Fatal(s.Name, result, err)
			}
		}
	}
	for _, field := range []struct{ table, column string }{{"telemetry_samples", "sampled_at"}, {"telemetry_rollups_5m", "bucket_at"}, {"telemetry_rollups_1h", "bucket_at"}} {
		rows, err := b.Query(ctx, `SELECT `+field.column+` FROM `+field.table+` ORDER BY `+field.column)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for rows.Next() {
			var n int64
			if err = rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			got = append(got, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(times) {
			t.Fatalf("%s rows lost", field.table)
		}
		for i, at := range times {
			want, err := telemetryMicros(at)
			if err != nil || want != got[i] {
				t.Fatalf("%s historical timestamp changed: %d != %d (%v)", field.table, got[i], want, err)
			}
		}
	}
}
