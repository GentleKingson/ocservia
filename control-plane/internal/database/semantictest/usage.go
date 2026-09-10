package semantictest

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
	"github.com/google/uuid"
)

type UsageTotal struct {
	Period     string
	Start      value.Timestamp
	ObservedAt value.Timestamp
	RX, TX     int64
}

// UsageHarness supplies only backend-specific fixture/inspection SQL. Every
// mutation under test runs the same production userusage business function.
type UsageHarness struct {
	Backend     database.Backend
	SeedNode    func(*testing.T) uuid.UUID
	ReadTotals  func(context.Context, uuid.UUID) ([]UsageTotal, error)
	CursorCount func(context.Context, uuid.UUID) (int, error)
}

func UsageTransactions(t *testing.T, h UsageHarness) {
	now := time.Date(2026, 9, 30, 23, 59, 59, 123456000, time.UTC)
	base := userusage.Sample{SessionID: strings.Repeat("s", 256), Username: "user", Connected: now.Add(-time.Hour), ObservedAt: now, RXBytes: 10, TXBytes: 20}
	write := func(ctx context.Context, node uuid.UUID, samples []userusage.Sample, after func() error) error {
		return database.Within(ctx, h.Backend, database.ReadCommitted, func(tx database.Tx) error {
			if err := userusage.RecordTransaction(ctx, tx, node, samples); err != nil {
				return err
			}
			if after != nil {
				return after()
			}
			return nil
		})
	}
	stamp := func(at time.Time) value.Timestamp {
		t.Helper()
		v, err := value.FromTime(at)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	withStore := func(change func(userusage.Store) error) error {
		return database.Within(context.Background(), h.Backend, database.ReadCommitted, func(tx database.Tx) error {
			p, ok := tx.(interface{ UsageStore() userusage.Store })
			if !ok {
				return database.ErrUnsupported
			}
			return change(p.UsageStore())
		})
	}
	t.Run("logical_cursor_and_period_keys", func(t *testing.T) {
		node := h.SeedNode(t)
		clocks := []int64{value.NegativeInfinity, value.MinTimestamp, value.EndTimestamp - 1, value.PositiveInfinity}
		for _, micros := range clocks {
			at := value.Timestamp{Valid: true, Micros: micros}
			cursor := userusage.Cursor{SessionID: base.SessionID, Username: base.Username, Connected: at, ObservedAt: at, RXBytes: 10, TXBytes: 20}
			if err := withStore(func(store userusage.Store) error {
				if err := store.LockNode(context.Background(), node); err != nil {
					return err
				}
				if err := store.PutCursor(context.Background(), node, cursor); err != nil {
					return err
				}
				read, err := store.LockCursor(context.Background(), node, cursor)
				if err != nil || read != cursor {
					t.Fatalf("logical cursor %+v != %+v: %v", read, cursor, err)
				}
				return store.AddUsage(context.Background(), node, cursor, "monthly", at, 10, 20)
			}); err != nil {
				t.Fatal(err)
			}
		}
		count, err := h.CursorCount(context.Background(), node)
		if err != nil || count != len(clocks) {
			t.Fatal("distinct logical keys", count, err)
		}
		totals, err := h.ReadTotals(context.Background(), node)
		if err != nil || len(totals) != len(clocks) {
			t.Fatal("logical periods", totals, err)
		}
		seen := make(map[int64]bool)
		for _, total := range totals {
			if !total.Start.Valid || total.ObservedAt != total.Start || total.RX != 10 || total.TX != 20 || seen[total.Start.Micros] {
				t.Fatal("logical total", total)
			}
			seen[total.Start.Micros] = true
		}
		for _, micros := range clocks {
			if !seen[micros] {
				t.Fatal("missing logical period", micros)
			}
		}
	})
	t.Run("extended_cursor_observation_order", func(t *testing.T) {
		for _, micros := range []int64{value.NegativeInfinity, value.MinTimestamp, value.EndTimestamp - 1, value.PositiveInfinity} {
			node := h.SeedNode(t)
			cursor := userusage.Cursor{SessionID: base.SessionID, Username: base.Username, Connected: stamp(base.Connected), ObservedAt: value.Timestamp{Valid: true, Micros: micros}, RXBytes: 5, TXBytes: 7}
			if err := withStore(func(store userusage.Store) error { return store.PutCursor(context.Background(), node, cursor) }); err != nil {
				t.Fatal(err)
			}
			if err := write(context.Background(), node, []userusage.Sample{base}, nil); err != nil {
				t.Fatal(err)
			}
			totals, err := h.ReadTotals(context.Background(), node)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if micros < stamp(base.ObservedAt).Micros {
				want = 2
				cursor.ObservedAt = stamp(base.ObservedAt)
				cursor.RXBytes, cursor.TXBytes = 10, 20
			}
			if len(totals) != want {
				t.Fatal("extended cursor ordering", micros, totals)
			}
			for _, total := range totals {
				if total.RX != 5 || total.TX != 13 || total.ObservedAt != stamp(base.ObservedAt) {
					t.Fatal("extended cursor delta", total)
				}
			}
			if err := withStore(func(store userusage.Store) error {
				got, err := store.LockCursor(context.Background(), node, cursor)
				if err != nil || got != cursor {
					t.Fatal("stored observation order", got, cursor, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("extended_finite_ingestion", func(t *testing.T) {
		for _, year := range []int{-1000, 12000} {
			node := h.SeedNode(t)
			sample := base
			sample.Connected = time.Date(year, 6, 7, 0, 0, 0, 123456000, time.UTC)
			sample.ObservedAt = sample.Connected.Add(time.Hour)
			if err := write(context.Background(), node, []userusage.Sample{sample, sample}, nil); err != nil {
				t.Fatal(err)
			}
			totals, err := h.ReadTotals(context.Background(), node)
			if err != nil || len(totals) != 2 {
				t.Fatal("extended ingestion", totals, err)
			}
			for _, total := range totals {
				start := time.Unix(0, 0).UTC()
				if total.Period == "monthly" {
					start = time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC)
				}
				if total.Start != stamp(start) || total.ObservedAt != stamp(sample.ObservedAt) || total.RX != 10 || total.TX != 20 {
					t.Fatal("extended ingestion total", total)
				}
			}
		}
	})
	check := func(t *testing.T, node uuid.UUID, cursors int, want map[string][2]int64) {
		t.Helper()
		count, err := h.CursorCount(context.Background(), node)
		if err != nil || count != cursors {
			t.Fatalf("cursor count %d, want %d: %v", count, cursors, err)
		}
		totals, err := h.ReadTotals(context.Background(), node)
		if err != nil {
			t.Fatal(err)
		}
		if len(totals) != len(want) {
			t.Fatalf("totals: %+v, want %v", totals, want)
		}
		for _, total := range totals {
			start, err := total.Start.Time()
			if err != nil {
				t.Fatal(err)
			}
			key := total.Period + ":" + start.Format("2006-01-02")
			value, ok := want[key]
			if !ok || value != [2]int64{total.RX, total.TX} {
				t.Fatalf("unexpected total %+v", total)
			}
		}
	}
	t.Run("delta_replay_reset_month_boundary", func(t *testing.T) {
		node := h.SeedNode(t)
		second := base
		second.ObservedAt, second.RXBytes, second.TXBytes = now.Add(time.Second), 15, 27
		reset := second
		reset.ObservedAt, reset.RXBytes, reset.TXBytes = now.Add(2*time.Second), 3, 4
		stale := base
		stale.RXBytes = 999
		for _, samples := range [][]userusage.Sample{{base}, {base, second, second}, {reset, stale}} {
			if err := write(context.Background(), node, samples, nil); err != nil {
				t.Fatal(err)
			}
		}
		check(t, node, 1, map[string][2]int64{"monthly:2026-09-01": {10, 20}, "monthly:2026-10-01": {8, 11}, "lifetime:1970-01-01": {18, 31}})
	})
	t.Run("saturation", func(t *testing.T) {
		node := h.SeedNode(t)
		first, second := base, base
		first.RXBytes, first.TXBytes = math.MaxInt64-1, math.MaxInt64
		second.SessionID = "another"
		if err := write(context.Background(), node, []userusage.Sample{first, second}, nil); err != nil {
			t.Fatal(err)
		}
		check(t, node, 2, map[string][2]int64{"monthly:2026-09-01": {math.MaxInt64, math.MaxInt64}, "lifetime:1970-01-01": {math.MaxInt64, math.MaxInt64}})
	})
	t.Run("microsecond_replay_order", func(t *testing.T) {
		node := h.SeedNode(t)
		first, duplicate, next := base, base, base
		first.ObservedAt = base.ObservedAt.Add(time.Nanosecond)
		duplicate.ObservedAt, duplicate.RXBytes, duplicate.TXBytes = base.ObservedAt.Add(999*time.Nanosecond), 100, 200
		next.ObservedAt, next.RXBytes, next.TXBytes = base.ObservedAt.Add(time.Microsecond), 12, 23
		if err := write(context.Background(), node, []userusage.Sample{first, duplicate, next, next}, nil); err != nil {
			t.Fatal(err)
		}
		check(t, node, 1, map[string][2]int64{"monthly:2026-09-01": {12, 23}, "lifetime:1970-01-01": {12, 23}})
	})
	t.Run("username_change_rolls_back_whole_batch", func(t *testing.T) {
		node := h.SeedNode(t)
		changed := base
		changed.Username, changed.ObservedAt = "other", now.Add(time.Second)
		if err := write(context.Background(), node, []userusage.Sample{base, changed}, nil); !errors.Is(err, userusage.ErrInvalidSample) {
			t.Fatal(err)
		}
		check(t, node, 0, map[string][2]int64{})
	})
	t.Run("request_cancel_and_business_error", func(t *testing.T) {
		for _, cancelRequest := range []bool{false, true} {
			node := h.SeedNode(t)
			ctx, cancel := context.WithCancel(context.Background())
			failure := errors.New("later business write failed")
			err := write(ctx, node, []userusage.Sample{base}, func() error {
				if cancelRequest {
					cancel()
					return ctx.Err()
				}
				return failure
			})
			cancel()
			want := failure
			if cancelRequest {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			check(t, node, 0, map[string][2]int64{})
		}
	})
	t.Run("concurrent_first_observation", func(t *testing.T) {
		node := h.SeedNode(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		results := make(chan error, 4)
		for range 4 {
			go func() { results <- write(ctx, node, []userusage.Sample{base}, nil) }()
		}
		for range 4 {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
		check(t, node, 1, map[string][2]int64{"monthly:2026-09-01": {10, 20}, "lifetime:1970-01-01": {10, 20}})
	})
}
