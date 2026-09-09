package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/google/uuid"
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
	pool, err := pgxpool.New(ctx, dsn)
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
		for _, query := range []string{`DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, `DELETE FROM telemetry_rollups_1h WHERE node_id=$1`, `DELETE FROM nodes WHERE id=$1`} {
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
}
