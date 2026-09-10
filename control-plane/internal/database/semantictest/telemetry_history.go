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
	if err := h.DeleteBatch(); err != nil {
		t.Fatal(err)
	}
	if got := read("raw", beforeEpoch.Add(-time.Second)); len(got) != 0 {
		t.Fatal("ingest-batch cascade lost", got)
	}
}
