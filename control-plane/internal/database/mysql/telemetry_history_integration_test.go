package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestRealTelemetryHistoryWorkflow(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := owner.ProvisionTelemetryMonth(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := owner.MigrateTelemetryHistory(ctx); err != nil {
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
	workspace, node, batch := uuid.New(), uuid.New(), uuid.New()
	if _, err = owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'history',?,CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(workspace), workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'history','active',CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(node), UUIDBytes(workspace)); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES(?,?,1,'raw_history',?,1)`, UUIDBytes(batch), UUIDBytes(node), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	semantictest.TelemetryHistoryWorkflow(t, semantictest.TelemetryHistoryHarness{
		Backend: b, Now: now, Node: node, Batch: batch,
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
}
