package store

import (
	"context"
	"errors"
	"maps"
	"math"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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

func (s *memoryUsageStore) LockCursor(_ context.Context, _ uuid.UUID, sample UsageCursor) (UsageCursor, error) {
	s.calls++
	key := UsageKey{SessionID: sample.SessionID, Connected: sample.Connected.UTC().Truncate(time.Microsecond)}
	cursor, ok := s.cursors[key]
	if !ok {
		return UsageCursor{}, database.ErrNotFound
	}
	return cursor, nil
}

func (s *memoryUsageStore) PutCursor(_ context.Context, _ uuid.UUID, sample UsageCursor) error {
	s.calls++
	key := UsageKey{SessionID: sample.SessionID, Connected: sample.Connected.UTC().Truncate(time.Microsecond)}
	prior, ok := s.cursors[key]
	if !ok || sample.ObservedAt.After(prior.ObservedAt) {
		s.cursors[key] = sample
	}
	return nil
}

func (s *memoryUsageStore) AddUsage(_ context.Context, _ uuid.UUID, sample UsageCursor, period string, start time.Time, rx, tx int64) error {
	s.calls++
	key := memoryUsageKey{username: sample.Username, period: period, periodStart: start}
	prev := s.totals[key]
	prev.rx = saturatingAddInt64(prev.rx, rx)
	prev.tx = saturatingAddInt64(prev.tx, tx)
	if sample.ObservedAt.After(prev.observed) {
		prev.observed = sample.ObservedAt
	}
	s.totals[key] = prev
	return nil
}

type memoryBatchUsageStore struct {
	*memoryUsageStore
}

func (s *memoryBatchUsageStore) LockCursors(_ context.Context, _ uuid.UUID, keys []UsageKey) (map[UsageKey]UsageCursor, error) {
	s.calls++
	out := make(map[UsageKey]UsageCursor, len(keys))
	for _, key := range keys {
		if cursor, ok := s.cursors[key]; ok {
			out[key] = cursor
		}
	}
	return out, nil
}

func (s *memoryBatchUsageStore) PutCursors(_ context.Context, _ uuid.UUID, cursors []UsageCursor) error {
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

func (s *memoryBatchUsageStore) AddUsageBatch(_ context.Context, _ uuid.UUID, deltas []UsageDelta) error {
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

func TestRecordUsageBatchMatchesPerSampleSemantics(t *testing.T) {
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

	single := newMemoryUsageStore()
	batched := &memoryBatchUsageStore{memoryUsageStore: newMemoryUsageStore()}

	if err := RecordUsageTx(ctx, single, nodeID, seed); err != nil {
		t.Fatal(err)
	}
	if err := RecordUsageTx(ctx, batched, nodeID, seed); err != nil {
		t.Fatal(err)
	}
	single.calls = 0
	batched.calls = 0

	if err := RecordUsageTx(ctx, single, nodeID, batch); err != nil {
		t.Fatal(err)
	}
	if err := RecordUsageTx(ctx, batched, nodeID, batch); err != nil {
		t.Fatal(err)
	}

	if !maps.Equal(single.cursors, batched.cursors) {
		t.Fatalf("cursor mismatch:\nsingle =%+v\nbatched=%+v", single.cursors, batched.cursors)
	}
	if !maps.Equal(single.totals, batched.totals) {
		t.Fatalf("totals mismatch:\nsingle =%+v\nbatched=%+v", single.totals, batched.totals)
	}
	if batched.calls != 3 {
		t.Fatalf("expected 3 batched store calls (LockCursors/PutCursors/AddUsageBatch), got %d", batched.calls)
	}
	if single.calls <= batched.calls {
		t.Fatalf("expected per-sample calls (%d) > batched calls (%d)", single.calls, batched.calls)
	}

	// Username mismatch on newer observation fails closed.
	err := RecordUsageTx(ctx, batched, nodeID, []UsageSample{
		{SessionID: "s1", Username: "mallory", Connected: t0, RXBytes: 50, TXBytes: 50, ObservedAt: t0.Add(4 * time.Minute)},
	})
	if !errors.Is(err, ErrInvalidUsageSample) {
		t.Fatalf("expected ErrInvalidUsageSample, got %v", err)
	}
}
