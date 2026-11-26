package store

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var ErrInvalidUsageSample = errors.New("usage sample is invalid")

type UsageSample struct {
	SessionID  string
	Username   string
	Connected  time.Time
	RXBytes    int64
	TXBytes    int64
	ObservedAt time.Time
}

type UsageCursor struct {
	SessionID, Username   string
	Connected, ObservedAt time.Time
	RXBytes, TXBytes      int64
}

type UsageKey struct {
	SessionID string
	Connected time.Time
}

type UsageDelta struct {
	Username    string
	Period      string
	PeriodStart time.Time
	RXBytes     int64
	TXBytes     int64
	ObservedAt  time.Time
}

// UsageStore executes usage cursor locking, cursor upserts, and usage
// aggregation in bulk queries.
type UsageStore interface {
	LockNode(context.Context, uuid.UUID) error
	LockCursors(context.Context, uuid.UUID, []UsageKey) (map[UsageKey]UsageCursor, error)
	PutCursors(context.Context, uuid.UUID, []UsageCursor) error
	AddUsageBatch(context.Context, uuid.UUID, []UsageDelta) error
}

// RecordUsageTransaction resolves the usage store from the caller's transaction,
// retaining node-before-cursor lock ordering.
func RecordUsageTransaction(ctx context.Context, tx database.Tx, nodeID uuid.UUID, samples []UsageSample) error {
	provider, ok := tx.(interface{ UsageStore() UsageStore })
	if !ok {
		return database.ErrUnsupported
	}
	if len(samples) == 0 {
		return nil
	}
	usage := provider.UsageStore()
	if err := usage.LockNode(ctx, nodeID); err != nil {
		return err
	}
	return RecordUsageTx(ctx, usage, nodeID, samples)
}

func RecordUsageTx(ctx context.Context, store UsageStore, nodeID uuid.UUID, samples []UsageSample) error {
	if len(samples) == 0 {
		return nil
	}
	keys := make([]UsageKey, 0, len(samples))
	seenKeys := make(map[UsageKey]struct{}, len(samples))
	for _, sample := range samples {
		if sample.RXBytes < 0 || sample.TXBytes < 0 || sample.ObservedAt.IsZero() || sample.Connected.IsZero() {
			return ErrInvalidUsageSample
		}
		key := UsageKey{SessionID: sample.SessionID, Connected: sample.Connected.UTC().Truncate(time.Microsecond)}
		if _, seen := seenKeys[key]; !seen {
			seenKeys[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, compareUsageKey)
	cursors, err := store.LockCursors(ctx, nodeID, keys)
	if err != nil {
		return err
	}
	if cursors == nil {
		cursors = make(map[UsageKey]UsageCursor, len(keys))
	}

	type deltaKey struct {
		username    string
		period      string
		periodStart time.Time
	}
	deltas := make(map[deltaKey]*UsageDelta, len(keys)*2)
	dirtyCursors := make(map[UsageKey]UsageCursor, len(keys))

	for _, sample := range samples {
		connected := sample.Connected.UTC().Truncate(time.Microsecond)
		observed := sample.ObservedAt.UTC().Truncate(time.Microsecond)
		key := UsageKey{SessionID: sample.SessionID, Connected: connected}
		current := UsageCursor{SessionID: sample.SessionID, Username: sample.Username, Connected: connected, ObservedAt: observed, RXBytes: sample.RXBytes, TXBytes: sample.TXBytes}
		prior, hasPrior := cursors[key]
		if hasPrior && !observed.After(prior.ObservedAt) {
			continue
		}
		if hasPrior && sample.Username != prior.Username {
			return ErrInvalidUsageSample
		}
		deltaRX, deltaTX := sample.RXBytes-prior.RXBytes, sample.TXBytes-prior.TXBytes
		if deltaRX < 0 {
			deltaRX = sample.RXBytes
		}
		if deltaTX < 0 {
			deltaTX = sample.TXBytes
		}
		cursors[key] = current
		dirtyCursors[key] = current
		for _, period := range []struct {
			kind  string
			start time.Time
		}{{"monthly", monthStart(sample.ObservedAt)}, {"lifetime", time.Unix(0, 0).UTC()}} {
			dk := deltaKey{username: sample.Username, period: period.kind, periodStart: period.start}
			agg, exists := deltas[dk]
			if !exists {
				agg = &UsageDelta{Username: sample.Username, Period: period.kind, PeriodStart: period.start}
				deltas[dk] = agg
			}
			agg.RXBytes = saturatingAddInt64(agg.RXBytes, deltaRX)
			agg.TXBytes = saturatingAddInt64(agg.TXBytes, deltaTX)
			if observed.After(agg.ObservedAt) {
				agg.ObservedAt = observed
			}
		}
	}
	if len(dirtyCursors) == 0 {
		return nil
	}
	cursorRows := make([]UsageCursor, 0, len(dirtyCursors))
	for _, cursor := range dirtyCursors {
		cursorRows = append(cursorRows, cursor)
	}
	slices.SortFunc(cursorRows, func(a, b UsageCursor) int {
		return compareUsageKey(UsageKey{SessionID: a.SessionID, Connected: a.Connected}, UsageKey{SessionID: b.SessionID, Connected: b.Connected})
	})
	if err := store.PutCursors(ctx, nodeID, cursorRows); err != nil {
		return err
	}
	usageRows := make([]UsageDelta, 0, len(deltas))
	for _, delta := range deltas {
		usageRows = append(usageRows, *delta)
	}
	slices.SortFunc(usageRows, compareUsageDelta)
	return store.AddUsageBatch(ctx, nodeID, usageRows)
}

func compareUsageKey(a, b UsageKey) int {
	if c := cmp.Compare(a.SessionID, b.SessionID); c != 0 {
		return c
	}
	return a.Connected.Compare(b.Connected)
}

func compareUsageDelta(a, b UsageDelta) int {
	if c := cmp.Compare(a.Username, b.Username); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Period, b.Period); c != 0 {
		return c
	}
	return a.PeriodStart.Compare(b.PeriodStart)
}

func saturatingAddInt64(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

func monthStart(v time.Time) time.Time {
	v = v.UTC()
	return time.Date(v.Year(), v.Month(), 1, 0, 0, 0, 0, time.UTC)
}
