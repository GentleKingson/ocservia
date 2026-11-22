package database

import (
	"context"
	"time"
)

// TransactionTime is stable for the lifetime of the transaction. Stores must
// use it for logical now() defaults, never a Go process clock or statement NOW.
func TransactionTime(ctx context.Context, tx Tx) (time.Time, error) {
	clock, ok := tx.(interface {
		TransactionTime(context.Context) (time.Time, error)
	})
	if !ok {
		return time.Time{}, ErrUnsupported
	}
	return clock.TransactionTime(ctx)
}

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
