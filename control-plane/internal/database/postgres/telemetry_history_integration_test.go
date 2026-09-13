package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTelemetryHistoryWorkflowIntegration(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ownerDSN := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerDSN == "" {
		t.Fatal("OCSERV_TEST_OWNER_DATABASE_URL is required for fixture cleanup and the foreign-key cascade assertion")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// A fractional-hour offset exposes accidental session-local rollup origins.
	config.ConnConfig.RuntimeParams["timezone"] = "Asia/Kathmandu"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	b := WrapPool(pool)
	ownerPool, err := pgxpool.New(ctx, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	owner := WrapPool(ownerPool)
	t.Run("telemetry-privilege-upgrade", func(t *testing.T) {
		var role string
		if err := b.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `GRANT DELETE,TRUNCATE ON telemetry_rollups_5m,telemetry_rollups_1h TO `+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if err := b.ValidateTelemetryRuntime(ctx); !errors.Is(err, database.ErrPermission) {
			t.Fatalf("legacy unrestricted cleanup accepted: %v", err)
		}
		if err := migrations.GrantRuntimePrivileges(ctx, ownerPool, role); err != nil {
			t.Fatal(err)
		}
		if err := b.ValidateTelemetryRuntime(ctx); err != nil {
			t.Fatal(err)
		}
	})
	now := time.Now().UTC()
	workspace, node, batch := uuid.New(), uuid.New(), uuid.New()
	if _, err = b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'history',$2,now(),now())`, workspace, workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'history','active',now(),now())`, node, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES($1,$2,1,'raw_history',$3,1)`, batch, node, now); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, query := range []string{`DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, `DELETE FROM telemetry_rollups_5m WHERE node_id=$1`, `DELETE FROM telemetry_rollups_1h WHERE node_id=$1`, `DELETE FROM nodes WHERE id=$1`} {
			if _, err := owner.Exec(ctx, query, node); err != nil {
				t.Error(err)
			}
		}
		if _, err := owner.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspace); err != nil {
			t.Error(err)
		}
	}()
	semantictest.TelemetryHistoryWorkflow(t, semantictest.TelemetryHistoryHarness{
		Backend: b, Now: now, Node: node, Batch: batch,
		Prune: func(ctx context.Context, tx database.Tx, now time.Time) error {
			_, err := tx.Exec(ctx, `SELECT telemetry_prune_rollups($1)`, now)
			return err
		},
		SeedInfinity: func(at value.Timestamp) error {
			_, err := b.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES($1,$2,$3,'cpu_usage_ratio',7)`, node, batch, at)
			return err
		},
		SeedOldRaw: func(at time.Time) error {
			_, err := b.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES($1,$2,$3,'cpu_usage_ratio',1)`, node, batch, at)
			return err
		},
		SeedExpiredRollups: func(five, hour time.Time) error {
			for _, row := range []struct {
				suffix string
				at     time.Time
			}{{"5m", five}, {"1h", hour}} {
				if _, err := b.Exec(ctx, `INSERT INTO telemetry_rollups_`+row.suffix+`(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES($1,'cpu_usage_ratio',$2,1,1,1,1)`, node, row.at); err != nil {
					return err
				}
			}
			return nil
		},
		DeleteBatch: func() error {
			_, err := owner.Exec(ctx, `DELETE FROM telemetry_ingest_batches WHERE batch_id=$1`, batch)
			return err
		},
	})
	t.Run("partition-recovery-and-bounded-drop", func(t *testing.T) {
		batch := uuid.New()
		if _, err := b.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES($1,$2,2,'raw_history',$3,1)`, batch, node, now); err != nil {
			t.Fatal(err)
		}
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -3, 0)
		var names []string
		for i := range 2 {
			at := month.AddDate(0, i, 0).Add(time.Hour)
			start := month.AddDate(0, i, 0)
			name := "telemetry_samples_" + start.Format("200601")
			if _, err := owner.Exec(ctx, `CREATE TABLE `+pgx.Identifier{name}.Sanitize()+` PARTITION OF telemetry_samples FOR VALUES FROM ('`+start.Format(time.RFC3339)+`') TO ('`+start.AddDate(0, 1, 0).Format(time.RFC3339)+`')`); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES($1,$2,$3,'connection_rtt_ms',12)`, node, batch, at); err != nil {
				t.Fatal(err)
			}
			names = append(names, "telemetry_samples_"+at.Format("200601"))
		}
		exists := func(name string) bool {
			t.Helper()
			var found bool
			if err := owner.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&found); err != nil {
				t.Fatal(err)
			}
			return found
		}
		drop := func(rollback bool) error {
			return database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
				var dropped int
				if err := tx.QueryRow(ctx, `SELECT telemetry_drop_expired_partitions($1)`, now.Add(-14*24*time.Hour)).Scan(&dropped); err != nil {
					return err
				}
				if dropped != 1 {
					t.Fatalf("expected one dropped partition: %d", dropped)
				}
				if rollback {
					return context.Canceled
				}
				return nil
			})
		}
		if err := drop(true); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if !exists(names[0]) || !exists(names[1]) {
			t.Fatal("partition drop escaped rollback")
		}
		if err := drop(false); err != nil {
			t.Fatal(err)
		}
		if exists(names[0]) || !exists(names[1]) {
			t.Fatal("drop did not stop after one month")
		}
		if err := drop(false); err != nil {
			t.Fatal(err)
		}
		if exists(names[1]) {
			t.Fatal("drop did not resume")
		}
		var count int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM telemetry_rollups_1h WHERE node_id=$1 AND metric='connection_rtt_ms' AND sample_count=1 AND avg_value=12`, node).Scan(&count); err != nil || count != 2 {
			t.Fatalf("outage rollups missing: count=%d err=%v", count, err)
		}
	})
}
