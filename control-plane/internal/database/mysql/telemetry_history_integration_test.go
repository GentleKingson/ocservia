package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryhistory"
	"github.com/google/uuid"
)

func TestRealTelemetryHistoryWorkflow(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := owner.PrepareControllerTelemetry(ctx); err != nil {
		t.Fatal(err)
	}
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	options.DSN = strings.Replace(options.DSN, "ocservia_owner:pr02-owner-test-only@", "ocservia_app:pr02-runtime-test-only@", 1)
	b, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	t.Run("runtime-requires-active-month", func(t *testing.T) {
		if err := b.ValidateTelemetryRuntime(ctx); err != nil {
			t.Fatal(err)
		}
		name, _, _, err := telemetryMonth(now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `UPDATE telemetry_sample_shards SET state='retired' WHERE table_name=?`, name); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := owner.Exec(ctx, `UPDATE telemetry_sample_shards SET state='active' WHERE table_name=?`, name); err != nil {
				t.Error(err)
			}
		}()
		if err := b.ValidateTelemetryRuntime(ctx); !errors.Is(err, ErrSchema) {
			t.Fatalf("missing telemetry month accepted at startup: %v", err)
		}
	})
	workspace, node, batch := uuid.New(), uuid.New(), uuid.New()
	if _, err = owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'history',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, UUIDBytes(workspace), workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'history','active',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, UUIDBytes(node), UUIDBytes(workspace)); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES(?,?,1,'raw_history',?,1)`, UUIDBytes(batch), UUIDBytes(node), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	semantictest.TelemetryHistoryWorkflow(t, semantictest.TelemetryHistoryHarness{
		Backend: b, Now: now, Node: node, Batch: batch,
		Prune: func(ctx context.Context, tx database.Tx, now time.Time) error {
			at, err := value.FromTime(now)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `CALL telemetry_prune_rollups(?)`, at)
			return err
		},
		SeedInfinity: func(at value.Timestamp) error {
			_, err := owner.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,'cpu_usage_ratio',7)`, UUIDBytes(node), UUIDBytes(batch), at)
			return err
		},
		SeedOldRaw: func(at time.Time) error {
			v, err := value.FromTime(at)
			if err != nil {
				return err
			}
			if err = owner.ProvisionTelemetryMonth(ctx, at); err != nil {
				return err
			}
			if err = owner.GrantTestPrivileges(ctx); err != nil {
				return err
			}
			name, _, _, err := telemetryMonth(at)
			if err != nil {
				return err
			}
			_, err = owner.Exec(ctx, `INSERT INTO `+name+`(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,'cpu_usage_ratio',1)`, UUIDBytes(node), UUIDBytes(batch), v)
			return err
		},
		SeedExpiredRollups: func(five, hour time.Time) error {
			for _, row := range []struct {
				suffix string
				at     time.Time
			}{{"5m", five}, {"1h", hour}} {
				v, err := value.FromTime(row.at)
				if err != nil {
					return err
				}
				if _, err = owner.Exec(ctx, `INSERT INTO telemetry_rollups_`+row.suffix+`(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES(?,'cpu_usage_ratio',?,1,1,1,1)`, UUIDBytes(node), v); err != nil {
					return err
				}
			}
			return nil
		},
		DeleteBatch: func() error {
			_, err := owner.Exec(ctx, `DELETE FROM telemetry_ingest_batches WHERE batch_id=?`, UUIDBytes(batch))
			return err
		},
	})
	t.Run("retirement-recovery-and-collection", func(t *testing.T) {
		batch := uuid.New()
		if _, err := owner.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES(?,?,2,'raw_history',?,1)`, UUIDBytes(batch), UUIDBytes(node), fixtureTimestamp(t, now)); err != nil {
			t.Fatal(err)
		}
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -3, 0)
		var names []string
		for i := range 2 {
			at := month.AddDate(0, i, 0).Add(time.Hour)
			if err := owner.ProvisionTelemetryMonth(ctx, at); err != nil {
				t.Fatal(err)
			}
			name, _, _, err := telemetryMonth(at)
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
			if _, err := owner.Exec(ctx, `INSERT INTO `+name+`(node_id,batch_id,sampled_at,metric,value) VALUES(?,?,?,'connection_rtt_ms',12)`, UUIDBytes(node), UUIDBytes(batch), fixtureTimestamp(t, at)); err != nil {
				t.Fatal(err)
			}
		}
		if err := owner.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		state := func(name string) string {
			t.Helper()
			var state string
			if err := owner.QueryRow(ctx, `SELECT state FROM telemetry_sample_shards WHERE table_name=?`, name).Scan(&state); err != nil {
				t.Fatal(err)
			}
			return state
		}
		maintain := func(rollback bool) error {
			return database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
				s, err := telemetryhistory.FromTransaction(tx)
				if err != nil {
					return err
				}
				if err := s.Maintain(ctx, now); err != nil {
					return err
				}
				if rollback {
					return context.Canceled
				}
				return nil
			})
		}
		if err := maintain(true); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if state(names[0]) != "active" || state(names[1]) != "active" {
			t.Fatal("retirement escaped rollback")
		}
		if err := maintain(false); err != nil {
			t.Fatal(err)
		}
		if state(names[0]) != "retired" || state(names[1]) != "active" {
			t.Fatal("retirement did not stop after one month")
		}
		if err := maintain(false); err != nil {
			t.Fatal(err)
		}
		if state(names[1]) != "retired" {
			t.Fatal("retirement did not resume")
		}
		var count int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM telemetry_rollups_1h WHERE node_id=? AND metric='connection_rtt_ms' AND sample_count=1 AND avg_value=12`, UUIDBytes(node)).Scan(&count); err != nil || count != 2 {
			t.Fatalf("outage rollups missing: count=%d err=%v", count, err)
		}
		// The earlier shared history fixture also retired its 1969 month.
		for attempts := 0; attempts < 3 && state(names[0]) != "dropped"; attempts++ {
			if err := owner.CollectRetiredTelemetryShards(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if state(names[0]) != "dropped" {
			t.Fatal("collector did not reclaim first month")
		}
		if state(names[1]) != "retired" {
			t.Fatal("collector exceeded one physical month")
		}
		// Simulate interruption after implicit-commit DROP but before the
		// catalog receipt update; the next owner invocation must recover.
		if _, err := owner.Exec(ctx, `DROP TABLE `+names[1]); err != nil {
			t.Fatal(err)
		}
		if err := owner.CollectRetiredTelemetryShards(ctx); err != nil {
			t.Fatal(err)
		}
		if state(names[1]) != "dropped" {
			t.Fatal("collector did not resume")
		}
		for _, name := range names {
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, name).Scan(&count); err != nil || count != 0 {
				t.Fatalf("physical month remains: %s count=%d err=%v", name, count, err)
			}
		}
	})
}
