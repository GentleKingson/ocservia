package store

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
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
	Connected, ObservedAt value.Timestamp
	RXBytes, TXBytes      int64
}

type UsageStore interface {
	LockNode(context.Context, uuid.UUID) error
	LockCursor(context.Context, uuid.UUID, UsageCursor) (UsageCursor, error)
	PutCursor(context.Context, uuid.UUID, UsageCursor) error
	AddUsage(context.Context, uuid.UUID, UsageCursor, string, value.Timestamp, int64, int64) error
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
	for _, sample := range samples {
		if sample.RXBytes < 0 || sample.TXBytes < 0 || sample.ObservedAt.IsZero() || sample.Connected.IsZero() {
			return ErrInvalidUsageSample
		}
		connected, err := value.FromTime(sample.Connected)
		if err != nil {
			return ErrInvalidUsageSample
		}
		observed, err := value.FromTime(sample.ObservedAt)
		if err != nil {
			return ErrInvalidUsageSample
		}
		current := UsageCursor{SessionID: sample.SessionID, Username: sample.Username, Connected: connected, ObservedAt: observed, RXBytes: sample.RXBytes, TXBytes: sample.TXBytes}
		prior, err := store.LockCursor(ctx, nodeID, current)
		if err != nil && !errors.Is(err, database.ErrNotFound) {
			return err
		}
		if err == nil && observed.Micros <= prior.ObservedAt.Micros {
			continue
		}
		if err == nil && sample.Username != prior.Username {
			return ErrInvalidUsageSample
		}
		deltaRX, deltaTX := sample.RXBytes-prior.RXBytes, sample.TXBytes-prior.TXBytes
		if deltaRX < 0 {
			deltaRX = sample.RXBytes
		}
		if deltaTX < 0 {
			deltaTX = sample.TXBytes
		}
		if err := store.PutCursor(ctx, nodeID, current); err != nil {
			return err
		}
		for _, period := range []struct {
			kind  string
			start time.Time
		}{{"monthly", monthStart(sample.ObservedAt)}, {"lifetime", time.Unix(0, 0).UTC()}} {
			start, err := value.FromTime(period.start)
			if err != nil {
				return ErrInvalidUsageSample
			}
			if err := store.AddUsage(ctx, nodeID, current, period.kind, start, deltaRX, deltaTX); err != nil {
				return err
			}
		}
	}
	return nil
}

func monthStart(v time.Time) time.Time {
	v = v.UTC()
	return time.Date(v.Year(), v.Month(), 1, 0, 0, 0, 0, time.UTC)
}
