package database

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

// TransactionTime is stable for the lifetime of the transaction. Stores must
// use it for logical now() defaults, never a Go process clock or statement NOW.
func TransactionTime(ctx context.Context, tx Tx) (value.Timestamp, error) {
	clock, ok := tx.(interface {
		TransactionTime(context.Context) (value.Timestamp, error)
	})
	if !ok {
		return value.Timestamp{}, ErrUnsupported
	}
	return clock.TransactionTime(ctx)
}

// WallTime must be read after acquiring authority locks. TransactionTime can
// predate a lock wait and must not authorize an expired lease's renewal.
func WallTime(ctx context.Context, tx Tx) (value.Timestamp, error) {
	clock, ok := tx.(interface {
		WallTime(context.Context) (value.Timestamp, error)
	})
	if !ok {
		return value.Timestamp{}, ErrUnsupported
	}
	return clock.WallTime(ctx)
}
