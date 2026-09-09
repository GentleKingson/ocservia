// Package userusage converts node session counters into durable quota usage.
package userusage

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var ErrInvalidSample = errors.New("usage sample is invalid")

type Sample struct {
	SessionID  string
	Username   string
	Connected  time.Time
	RXBytes    int64
	TXBytes    int64
	ObservedAt time.Time
}

// Store is bound to the caller's transaction. LockCursor holds its row lock
// until that transaction ends; neither write method may commit independently.
type Store interface {
	LockNode(context.Context, uuid.UUID) error
	LockCursor(context.Context, uuid.UUID, Sample) (Sample, error)
	PutCursor(context.Context, uuid.UUID, Sample) error
	AddUsage(context.Context, uuid.UUID, Sample, string, time.Time, int64, int64) error
}

// RecordTransaction resolves the backend's usage store from the exact shared
// transaction, not a pool. It retains telemetry's node-before-cursor lock order,
// including for a missing cursor. Reacquiring telemetry's existing node lock
// neither broadens its scope nor releases it before the outer commit.
func RecordTransaction(ctx context.Context, tx database.Tx, nodeID uuid.UUID, samples []Sample) error {
	provider, ok := tx.(interface{ UsageStore() Store })
	if !ok {
		return database.ErrUnsupported
	}
	if len(samples) == 0 {
		return nil
	}
	store := provider.UsageStore()
	if err := store.LockNode(ctx, nodeID); err != nil {
		return err
	}
	return RecordTx(ctx, store, nodeID, samples)
}

// RecordTx applies monotonically increasing session samples exactly once when
// the caller holds the node lock. Business callers use RecordTransaction to
// establish that lock, including before inserting the first cursor.
func RecordTx(ctx context.Context, store Store, nodeID uuid.UUID, samples []Sample) error {
	for _, sample := range samples {
		if sample.RXBytes < 0 || sample.TXBytes < 0 || sample.ObservedAt.IsZero() || sample.Connected.IsZero() {
			return ErrInvalidSample
		}
		prior, err := store.LockCursor(ctx, nodeID, sample)
		if err != nil && !errors.Is(err, database.ErrNotFound) {
			return err
		}
		if err == nil && !sample.ObservedAt.After(prior.ObservedAt) {
			continue
		}
		if err == nil && sample.Username != prior.Username {
			return ErrInvalidSample
		}
		deltaRX, deltaTX := sample.RXBytes-prior.RXBytes, sample.TXBytes-prior.TXBytes
		if deltaRX < 0 {
			deltaRX = sample.RXBytes
		}
		if deltaTX < 0 {
			deltaTX = sample.TXBytes
		}
		if err := store.PutCursor(ctx, nodeID, sample); err != nil {
			return err
		}
		for _, period := range []struct {
			kind  string
			start time.Time
		}{{"monthly", monthStart(sample.ObservedAt)}, {"lifetime", time.Unix(0, 0).UTC()}} {
			if err := store.AddUsage(ctx, nodeID, sample, period.kind, period.start, deltaRX, deltaTX); err != nil {
				return err
			}
		}
	}
	return nil
}

func monthStart(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
}
