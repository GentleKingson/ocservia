package semantictest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryhistory"
	"github.com/google/uuid"
)

type TelemetryHistoryHarness struct {
	Backend            database.Backend
	Now                time.Time
	Node, Batch        uuid.UUID
	SeedOldRaw         func(time.Time) error
	SeedInfinity       func(value.Timestamp) error
	SeedExpiredRollups func(time.Time, time.Time) error
	DeleteBatch        func() error
	Prune              func(context.Context, database.Tx, time.Time) error
}

// TelemetryHistoryWorkflow is the shared domain history chain. It deliberately
// does not stand in for authenticated Controller IngestWire acceptance.
func TelemetryHistoryWorkflow(t *testing.T, h TelemetryHistoryHarness) {
	t.Helper()
	ctx := context.Background()
	base := h.Now.UTC().Truncate(time.Hour)
	samples := []telemetryhistory.Sample{{SampledAt: base.Add(time.Second), Metric: "cpu_usage_ratio", Value: 2}, {SampledAt: base.Add(2 * time.Second), Metric: "cpu_usage_ratio", Value: 4}, {SampledAt: base.Add(301 * time.Second), Metric: "cpu_usage_ratio", Value: 9}}
	within := func(change func(telemetryhistory.Store) error) error {
		return database.Within(ctx, h.Backend, database.ReadCommitted, func(tx database.Tx) error {
			s, err := telemetryhistory.FromTransaction(tx)
			if err != nil {
				return err
			}
			return change(s)
		})
	}
	if err := within(func(s telemetryhistory.Store) error {
		if err := s.Insert(ctx, h.Node, h.Batch, samples); err != nil {
			return err
		}
		return s.Insert(ctx, h.Node, h.Batch, samples)
	}); err != nil {
		t.Fatal(err)
	}
	read := func(resolution string, since time.Time) []telemetryhistory.Point {
		var points []telemetryhistory.Point
		if err := within(func(s telemetryhistory.Store) error {
			at, err := value.FromTime(since)
			if err != nil {
				return err
			}
			points, err = s.History(ctx, h.Node, "cpu_usage_ratio", resolution, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return points
	}
	if got := read("raw", base); len(got) != 3 {
		t.Fatalf("replay duplicated samples: %+v", got)
	}
	if err := h.SeedExpiredRollups(h.Now.Add(-91*24*time.Hour), h.Now.AddDate(-2, 0, 0)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := within(func(s telemetryhistory.Store) error { return s.Maintain(ctx, h.Now) }); err != nil {
			t.Fatal(err)
		}
	}
	five := read("5m", h.Now.AddDate(-3, 0, 0))
	if len(five) != 2 || five[0].Count != 2 || five[0].Minimum != 2 || five[0].Maximum != 4 || five[0].Average != 3 || five[1].Count != 1 {
		t.Fatalf("5m rollup/retention mismatch: %+v", five)
	}
	hour := read("1h", h.Now.AddDate(-3, 0, 0))
	if len(hour) != 1 || hour[0].Count != 3 || hour[0].Average != 5 {
		t.Fatalf("1h rollup/retention mismatch: %+v", hour)
	}
	if err := within(func(s telemetryhistory.Store) error {
		if err := s.Insert(ctx, h.Node, h.Batch, []telemetryhistory.Sample{{SampledAt: base.Add(3 * time.Second), Metric: "cpu_usage_ratio", Value: 100}}); err != nil {
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
	if err := h.SeedOldRaw(beforeEpoch); err != nil {
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
		if err := h.SeedInfinity(at); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := within(func(s telemetryhistory.Store) error { return s.Maintain(ctx, h.Now) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := within(func(s telemetryhistory.Store) error {
		points, err := s.History(ctx, h.Node, "cpu_usage_ratio", "raw", negative)
		if err != nil {
			return err
		}
		// Retention may retire the old finite shard, but neither infinity is
		// a finite month and both must remain readable in their original order.
		if len(points) < 2 || points[0].At != negative || points[len(points)-1].At != positive {
			t.Fatalf("infinite raw timestamps lost: %+v", points)
		}
		for _, resolution := range []string{"raw", "5m", "1h"} {
			points, err = s.History(ctx, h.Node, "cpu_usage_ratio", resolution, positive)
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
	// A late arrival must update an already materialized bucket even when it
	// is older than 48 hours. The oldest recomputed bucket must stay complete.
	for _, resolution := range []struct {
		name, metric string
		width        time.Duration
	}{{"5m", "network_rx_bytes", 5 * time.Minute}, {"1h", "network_tx_bytes", time.Hour}} {
		t.Run("late-and-boundary-"+resolution.name, func(t *testing.T) {
			cutoff := h.Now.Add(-14 * 24 * time.Hour).Truncate(resolution.width)
			late := h.Now.Add(-7 * 24 * time.Hour).Truncate(resolution.width)
			insert := func(at time.Time, v float64) {
				t.Helper()
				if err := within(func(s telemetryhistory.Store) error {
					return s.Insert(ctx, h.Node, h.Batch, []telemetryhistory.Sample{{SampledAt: at, Metric: resolution.metric, Value: v}})
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
				if err := within(func(s telemetryhistory.Store) error { return s.Maintain(ctx, h.Now) }); err != nil {
					t.Fatal(err)
				}
				if err := within(func(s telemetryhistory.Store) error {
					at, _ := value.FromTime(cutoff)
					points, err := s.History(ctx, h.Node, resolution.metric, resolution.name, at)
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
			if _, err := h.Backend.Exec(ctx, `DELETE FROM `+table+` WHERE 1=0`); !errors.Is(err, database.ErrPermission) {
				t.Fatalf("runtime has unrestricted DELETE on %s: %v", table, err)
			}
		}
		for i := range 1001 {
			if err := h.SeedExpiredRollups(h.Now.Add(-91*24*time.Hour).Add(time.Duration(i)*time.Second), h.Now.AddDate(-2, 0, 0).Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		fiveBefore := len(read("5m", h.Now.AddDate(-3, 0, 0)))
		hourBefore := len(read("1h", h.Now.AddDate(-3, 0, 0)))
		prune := func(at time.Time, rollback bool) error {
			return database.Within(ctx, h.Backend, database.ReadCommitted, func(tx database.Tx) error {
				if err := h.Prune(ctx, tx, at); err != nil {
					return err
				}
				if rollback {
					return context.Canceled
				}
				return nil
			})
		}
		if err := prune(h.Now.Add(time.Hour), false); err == nil {
			t.Fatal("future cleanup cutoff accepted")
		}
		if err := prune(h.Now, true); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(read("5m", h.Now.AddDate(-3, 0, 0))) != fiveBefore || len(read("1h", h.Now.AddDate(-3, 0, 0))) != hourBefore {
			t.Fatal("cleanup escaped transaction rollback")
		}
		if err := prune(h.Now, false); err != nil {
			t.Fatal(err)
		}
		if len(read("5m", h.Now.AddDate(-3, 0, 0))) != fiveBefore-1000 || len(read("1h", h.Now.AddDate(-3, 0, 0))) != hourBefore-1000 {
			t.Fatal("cleanup did not respect the 1000-row bound")
		}
		if err := prune(h.Now, false); err != nil {
			t.Fatal(err)
		}
		if len(read("5m", h.Now.AddDate(-3, 0, 0))) != fiveBefore-1001 || len(read("1h", h.Now.AddDate(-3, 0, 0))) != hourBefore-1001 {
			t.Fatal("cleanup did not resume the remaining expired row")
		}
	})
	if err := h.DeleteBatch(); err != nil {
		t.Fatal(err)
	}
	if got := read("raw", beforeEpoch.Add(-time.Second)); len(got) != 0 {
		t.Fatal("ingest-batch cascade lost", got)
	}
}
