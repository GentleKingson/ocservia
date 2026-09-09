package semantictest

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
	"github.com/google/uuid"
)

type UsageTotal struct {
	Period string
	Start  time.Time
	RX, TX int64
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
			key := total.Period + ":" + total.Start.UTC().Format("2006-01-02")
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
