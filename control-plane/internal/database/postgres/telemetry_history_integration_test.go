package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
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
	if err := b.ValidateTelemetryRuntime(ctx); err != nil {
		t.Fatalf("runtime capabilities after owner migration: %v", err)
	}
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
		if _, err := b.Exec(ctx, `SELECT telemetry_ensure_month_partition(now())`); !errors.Is(err, database.ErrPermission) {
			t.Fatalf("runtime partition DDL capability remains: %v", err)
		}
		if _, err := owner.Exec(ctx, `GRANT EXECUTE ON FUNCTION telemetry_ensure_month_partition(timestamptz) TO `+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if err := b.ValidateTelemetryRuntime(ctx); !errors.Is(err, database.ErrPermission) {
			t.Fatalf("indirect DDL capability accepted: %v", err)
		}
		if err := migrations.GrantRuntimePrivileges(ctx, ownerPool, role); err != nil {
			t.Fatal(err)
		}
	})
	now := time.Now().UTC()
	t.Run("startup-requires-attached-month", func(t *testing.T) {
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		name := pgx.Identifier{"telemetry_samples_" + month.Format("200601")}.Sanitize()
		if _, err := owner.Exec(ctx, `ALTER TABLE telemetry_samples DETACH PARTITION `+name); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := owner.Exec(ctx, `ALTER TABLE telemetry_samples ATTACH PARTITION `+name+` FOR VALUES FROM ('`+month.Format(time.RFC3339)+`') TO ('`+month.AddDate(0, 1, 0).Format(time.RFC3339)+`')`); err != nil {
				t.Error(err)
			}
		}()
		if err := b.ValidateTelemetryRuntime(ctx); !errors.Is(err, database.ErrConstraint) {
			t.Fatalf("missing attached month accepted at startup: %v", err)
		}
	})
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
	t.Run("unprovisioned-month-fails-closed", func(t *testing.T) {
		at := now.AddDate(0, 6, 0)
		err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			s, err := telemetrystore.FromTransaction(tx)
			if err != nil {
				return err
			}
			return s.InsertHistory(ctx, node, batch, []telemetrystore.Sample{{SampledAt: at, Metric: "cpu_usage_ratio", Value: 1}})
		})
		if !errors.Is(err, database.ErrConstraint) {
			t.Fatalf("missing month did not fail closed: %v", err)
		}
		var count int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM ONLY telemetry_samples_default WHERE batch_id=$1 AND sampled_at=$2`, batch, at).Scan(&count); err != nil || count != 0 {
			t.Fatalf("sample leaked into default partition: %d %v", count, err)
		}
	})

	base := now.UTC().Truncate(time.Hour)
	samples := []telemetrystore.Sample{{SampledAt: base.Add(time.Second), Metric: "cpu_usage_ratio", Value: 2}, {SampledAt: base.Add(2 * time.Second), Metric: "cpu_usage_ratio", Value: 4}, {SampledAt: base.Add(301 * time.Second), Metric: "cpu_usage_ratio", Value: 9}}
	within := func(change func(telemetrystore.Store) error) error {
		return database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			s, err := telemetrystore.FromTransaction(tx)
			if err != nil {
				return err
			}
			return change(s)
		})
	}
	if err := within(func(s telemetrystore.Store) error {
		if err := s.InsertHistory(ctx, node, batch, samples); err != nil {
			return err
		}
		return s.InsertHistory(ctx, node, batch, samples)
	}); err != nil {
		t.Fatal(err)
	}
	read := func(resolution string, since time.Time) []telemetrystore.Point {
		var points []telemetrystore.Point
		if err := within(func(s telemetrystore.Store) error {
			at, err := value.FromTime(since)
			if err != nil {
				return err
			}
			points, err = s.History(ctx, node, "cpu_usage_ratio", resolution, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return points
	}
	if got := read("raw", base); len(got) != 3 {
		t.Fatalf("replay duplicated samples: %+v", got)
	}
	seedExpiredRollups := func(five, hour time.Time) error {
		for _, row := range []struct {
			suffix string
			at     time.Time
		}{{"5m", five}, {"1h", hour}} {
			if _, err := b.Exec(ctx, `INSERT INTO telemetry_rollups_`+row.suffix+`(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES($1,'cpu_usage_ratio',$2,1,1,1,1)`, node, row.at); err != nil {
				return err
			}
		}
		return nil
	}
	if err := seedExpiredRollups(now.Add(-91*24*time.Hour), now.AddDate(-2, 0, 0)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := within(func(s telemetrystore.Store) error { return s.MaintainHistory(ctx, now) }); err != nil {
			t.Fatal(err)
		}
	}
	five := read("5m", now.AddDate(-3, 0, 0))
	if len(five) != 2 || five[0].Count != 2 || five[0].Minimum != 2 || five[0].Maximum != 4 || five[0].Average != 3 || five[1].Count != 1 {
		t.Fatalf("5m rollup/retention mismatch: %+v", five)
	}
	hour := read("1h", now.AddDate(-3, 0, 0))
	if len(hour) != 1 || hour[0].Count != 3 || hour[0].Average != 5 {
		t.Fatalf("1h rollup/retention mismatch: %+v", hour)
	}
	if err := within(func(s telemetrystore.Store) error {
		if err := s.InsertHistory(ctx, node, batch, []telemetrystore.Sample{{SampledAt: base.Add(3 * time.Second), Metric: "cpu_usage_ratio", Value: 100}}); err != nil {
			return err
		}
		return context.Canceled
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := read("raw", base); len(got) != 3 {
		t.Fatal("cancelled transaction committed samples", got)
	}
	beforeEpoch := time.Unix(-1, 999999000).UTC()
	if _, err := owner.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES($1,$2,$3,'cpu_usage_ratio',1)`, node, batch, beforeEpoch); err != nil {
		t.Fatal(err)
	}
	old := read("raw", beforeEpoch.Add(-time.Second))
	expected, _ := value.FromTime(beforeEpoch)
	if len(old) != 4 || old[0].At != expected {
		t.Fatalf("signed microsecond history changed: %+v", old)
	}
	negative := value.Timestamp{Micros: value.NegativeInfinity, Valid: true}
	positive := value.Timestamp{Micros: value.PositiveInfinity, Valid: true}
	for _, at := range []value.Timestamp{negative, positive} {
		if _, err := owner.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES($1,$2,$3,'cpu_usage_ratio',7)`, node, batch, at); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := within(func(s telemetrystore.Store) error { return s.MaintainHistory(ctx, now) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := within(func(s telemetrystore.Store) error {
		points, err := s.History(ctx, node, "cpu_usage_ratio", "raw", negative)
		if err != nil {
			return err
		}
		if len(points) < 2 || points[0].At != negative || points[len(points)-1].At != positive {
			t.Fatalf("infinite raw timestamps lost: %+v", points)
		}
		for _, resolution := range []string{"raw", "5m", "1h"} {
			points, err = s.History(ctx, node, "cpu_usage_ratio", resolution, positive)
			if err != nil {
				return err
			}
			if len(points) != 1 || points[0].At != positive || points[0].Count != 1 || points[0].Average != 7 {
				t.Fatalf("%s infinite bucket changed: %+v", resolution, points)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, resolution := range []struct {
		name, metric string
		width        time.Duration
	}{{"5m", "network_rx_bytes", 5 * time.Minute}, {"1h", "network_tx_bytes", time.Hour}} {
		t.Run("late-and-boundary-"+resolution.name, func(t *testing.T) {
			cutoff := now.Add(-14 * 24 * time.Hour).Truncate(resolution.width)
			late := now.Add(-7 * 24 * time.Hour).Truncate(resolution.width)
			insert := func(at time.Time, v float64) {
				t.Helper()
				if err := within(func(s telemetrystore.Store) error {
					return s.InsertHistory(ctx, node, batch, []telemetrystore.Sample{{SampledAt: at, Metric: resolution.metric, Value: v}})
				}); err != nil {
					t.Fatal(err)
				}
			}
			insert(cutoff.Add(time.Second), 2)
			insert(cutoff.Add(resolution.width-time.Second), 4)
			insert(late.Add(time.Second), 2)
			for pass := range 3 {
				if pass == 1 {
					insert(late.Add(2*time.Second), 4)
				}
				if err := within(func(s telemetrystore.Store) error { return s.MaintainHistory(ctx, now) }); err != nil {
					t.Fatal(err)
				}
				if err := within(func(s telemetrystore.Store) error {
					at, _ := value.FromTime(cutoff)
					points, err := s.History(ctx, node, resolution.metric, resolution.name, at)
					if err != nil {
						return err
					}
					lateAt, _ := value.FromTime(late)
					found := 0
					for _, p := range points {
						if p.At != at && p.At != lateAt {
							continue
						}
						found++
						if p.At == lateAt && pass == 0 {
							if p.Count != 1 || p.Average != 2 {
								t.Fatalf("initial late bucket: %+v", p)
							}
						} else if p.Count != 2 || p.Minimum != 2 || p.Maximum != 4 || p.Average != 3 {
							t.Fatalf("partial or stale bucket: %+v", p)
						}
					}
					if found != 2 {
						t.Fatalf("missing boundary/late buckets: %+v", points)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Run("bounded-cleanup-and-permissions", func(t *testing.T) {
		for _, table := range []string{"telemetry_rollups_5m", "telemetry_rollups_1h"} {
			if _, err := b.Exec(ctx, `DELETE FROM `+table+` WHERE 1=0`); !errors.Is(err, database.ErrPermission) {
				t.Fatalf("runtime has unrestricted DELETE on %s: %v", table, err)
			}
		}
		for i := range 1001 {
			if err := seedExpiredRollups(now.Add(-91*24*time.Hour).Add(time.Duration(i)*time.Second), now.AddDate(-2, 0, 0).Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		fiveBefore := len(read("5m", now.AddDate(-3, 0, 0)))
		hourBefore := len(read("1h", now.AddDate(-3, 0, 0)))
		prune := func(at time.Time, rollback bool) error {
			return database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT telemetry_prune_rollups($1)`, at); err != nil {
					return err
				}
				if rollback {
					return context.Canceled
				}
				return nil
			})
		}
		if err := prune(now.Add(time.Hour), false); err == nil {
			t.Fatal("future cleanup cutoff accepted")
		}
		if err := prune(now, true); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(read("5m", now.AddDate(-3, 0, 0))) != fiveBefore || len(read("1h", now.AddDate(-3, 0, 0))) != hourBefore {
			t.Fatal("cleanup escaped transaction rollback")
		}
		if err := prune(now, false); err != nil {
			t.Fatal(err)
		}
		if len(read("5m", now.AddDate(-3, 0, 0))) != fiveBefore-1000 || len(read("1h", now.AddDate(-3, 0, 0))) != hourBefore-1000 {
			t.Fatal("cleanup did not respect the 1000-row bound")
		}
		if err := prune(now, false); err != nil {
			t.Fatal(err)
		}
		if len(read("5m", now.AddDate(-3, 0, 0))) != fiveBefore-1001 || len(read("1h", now.AddDate(-3, 0, 0))) != hourBefore-1001 {
			t.Fatal("cleanup did not resume the remaining expired row")
		}
	})
	if _, err := owner.Exec(ctx, `DELETE FROM telemetry_ingest_batches WHERE batch_id=$1`, batch); err != nil {
		t.Fatal(err)
	}
	if got := read("raw", beforeEpoch.Add(-time.Second)); len(got) != 0 {
		t.Fatal("ingest-batch cascade lost", got)
	}

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
