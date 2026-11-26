package store

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

type memoryUsageKey struct {
	username    string
	period      string
	periodStart time.Time
}

type memoryUsageTotals struct {
	rx       int64
	tx       int64
	observed time.Time
}

type memoryUsageStore struct {
	cursors map[UsageKey]UsageCursor
	totals  map[memoryUsageKey]memoryUsageTotals
	calls   int
}

func newMemoryUsageStore() *memoryUsageStore {
	return &memoryUsageStore{
		cursors: make(map[UsageKey]UsageCursor),
		totals:  make(map[memoryUsageKey]memoryUsageTotals),
	}
}

func (s *memoryUsageStore) LockNode(context.Context, uuid.UUID) error { return nil }

func (s *memoryUsageStore) LockCursors(_ context.Context, _ uuid.UUID, keys []UsageKey) (map[UsageKey]UsageCursor, error) {
	s.calls++
	out := make(map[UsageKey]UsageCursor, len(keys))
	for _, key := range keys {
		if cursor, ok := s.cursors[key]; ok {
			out[key] = cursor
		}
	}
	return out, nil
}

func (s *memoryUsageStore) PutCursors(_ context.Context, _ uuid.UUID, cursors []UsageCursor) error {
	s.calls++
	for _, cursor := range cursors {
		key := UsageKey{SessionID: cursor.SessionID, Connected: cursor.Connected.UTC().Truncate(time.Microsecond)}
		prior, ok := s.cursors[key]
		if !ok || cursor.ObservedAt.After(prior.ObservedAt) {
			s.cursors[key] = cursor
		}
	}
	return nil
}

func (s *memoryUsageStore) AddUsageBatch(_ context.Context, _ uuid.UUID, deltas []UsageDelta) error {
	s.calls++
	for _, d := range deltas {
		key := memoryUsageKey{username: d.Username, period: d.Period, periodStart: d.PeriodStart}
		prev := s.totals[key]
		prev.rx = saturatingAddInt64(prev.rx, d.RXBytes)
		prev.tx = saturatingAddInt64(prev.tx, d.TXBytes)
		if d.ObservedAt.After(prev.observed) {
			prev.observed = d.ObservedAt
		}
		s.totals[key] = prev
	}
	return nil
}

func TestRecordUsageBatchSemantics(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.New()
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	seed := []UsageSample{
		{SessionID: "s1", Username: "alice", Connected: t0, RXBytes: 100, TXBytes: 200, ObservedAt: t0.Add(time.Minute)},
		{SessionID: "s2", Username: "bob", Connected: t0, RXBytes: math.MaxInt64 - 50, TXBytes: 10, ObservedAt: t0.Add(time.Minute)},
	}
	batch := []UsageSample{
		// Stale replay (equal ObservedAt -> ignored)
		{SessionID: "s1", Username: "alice", Connected: t0, RXBytes: 999, TXBytes: 999, ObservedAt: t0.Add(time.Minute)},
		// Monotonic advance + duplicate session in same batch
		{SessionID: "s1", Username: "alice", Connected: t0, RXBytes: 160, TXBytes: 250, ObservedAt: t0.Add(2 * time.Minute)},
		// Counter reset within same batch
		{SessionID: "s1", Username: "alice", Connected: t0, RXBytes: 40, TXBytes: 30, ObservedAt: t0.Add(3 * time.Minute)},
		// Cross-month + overflow saturation check
		{SessionID: "s2", Username: "bob", Connected: t0, RXBytes: 200, TXBytes: 30, ObservedAt: t0.Add(30 * 24 * time.Hour)},
		// Brand-new session for existing user
		{SessionID: "s3", Username: "alice", Connected: t0.Add(5 * time.Minute), RXBytes: 25, TXBytes: 35, ObservedAt: t0.Add(6 * time.Minute)},
	}

	store := newMemoryUsageStore()
	if err := RecordUsageTx(ctx, store, nodeID, seed); err != nil {
		t.Fatal(err)
	}
	store.calls = 0

	if err := RecordUsageTx(ctx, store, nodeID, batch); err != nil {
		t.Fatal(err)
	}

	if store.calls != 3 {
		t.Fatalf("expected 3 batched store calls (LockCursors/PutCursors/AddUsageBatch), got %d", store.calls)
	}

	novStart := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	decStart := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	lifetime := time.Unix(0, 0).UTC()

	// alice: seed(100,200) + advance(60,50) + reset(40,30) + s3(25,35) = (225,315)
	if got := store.totals[memoryUsageKey{"alice", "monthly", novStart}]; got.rx != 225 || got.tx != 315 {
		t.Fatalf("alice monthly totals = %+v, want rx=225 tx=315", got)
	}
	if got := store.totals[memoryUsageKey{"alice", "lifetime", lifetime}]; got.rx != 225 || got.tx != 315 {
		t.Fatalf("alice lifetime totals = %+v, want rx=225 tx=315", got)
	}
	// bob: seed(MaxInt64-50, 10) + (rx reset 200, tx +20) in Dec -> monthly Dec=(200,20), lifetime rx=MaxInt64 tx=30
	if got := store.totals[memoryUsageKey{"bob", "monthly", novStart}]; got.rx != math.MaxInt64-50 || got.tx != 10 {
		t.Fatalf("bob nov totals = %+v", got)
	}
	if got := store.totals[memoryUsageKey{"bob", "monthly", decStart}]; got.rx != 200 || got.tx != 20 {
		t.Fatalf("bob dec totals = %+v, want rx=200 tx=20", got)
	}
	if got := store.totals[memoryUsageKey{"bob", "lifetime", lifetime}]; got.rx != math.MaxInt64 || got.tx != 30 {
		t.Fatalf("bob lifetime totals = %+v, want rx=MaxInt64 tx=30", got)
	}
	if c := store.cursors[UsageKey{"s1", t0}]; c.RXBytes != 40 || c.TXBytes != 30 {
		t.Fatalf("s1 final cursor = %+v, want rx=40 tx=30", c)
	}

	// Username mismatch on newer observation fails closed.
	err := RecordUsageTx(ctx, store, nodeID, []UsageSample{
		{SessionID: "s1", Username: "mallory", Connected: t0, RXBytes: 50, TXBytes: 50, ObservedAt: t0.Add(4 * time.Minute)},
	})
	if !errors.Is(err, ErrInvalidUsageSample) {
		t.Fatalf("expected ErrInvalidUsageSample, got %v", err)
	}
}
