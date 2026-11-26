package database

import (
	"context"
	"time"
)

// WallTime must be read after acquiring authority locks. TransactionTime can
// predate a lock wait and must not authorize an expired lease's renewal.
func WallTime(ctx context.Context, tx Tx) (time.Time, error) {
	clock, ok := tx.(interface {
		WallTime(context.Context) (time.Time, error)
	})
	if !ok {
		return time.Time{}, ErrUnsupported
	}
	return clock.WallTime(ctx)
}
