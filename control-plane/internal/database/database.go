// Package database defines the driver-free Controller database boundary.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Isolation uint8

const (
	DefaultIsolation Isolation = iota
	ReadCommitted
	RepeatableRead
	Serializable
)

var (
	ErrNotFound      = errors.New("database: not found")
	ErrUnique        = errors.New("database: unique constraint")
	ErrForeignKey    = errors.New("database: foreign key constraint")
	ErrConstraint    = errors.New("database: constraint")
	ErrSerialization = errors.New("database: serialization failure")
	ErrDeadlock      = errors.New("database: deadlock")
	ErrPermission    = errors.New("database: permission denied")
	ErrTxClosed      = errors.New("database: transaction closed")
	ErrTxAborted     = errors.New("database: transaction aborted")
	ErrUnsupported   = errors.New("database: unsupported operation")
	// ErrUnavailable describes connection availability, not a known rollback
	// or permission to replay a transaction callback.
	ErrUnavailable = errors.New("database: connection unavailable")
)

type PoolStats struct {
	Acquired, Idle, Total int64
}

// Tx is shared by every store participating in one business operation.
// Only the transaction owner commits or rolls back; stores never begin a Tx.
type Tx interface {
	Commit(context.Context) error
	Rollback(context.Context) error
	// WallTime must be read after acquiring authority locks. TransactionTime can
	// predate a lock wait and must not authorize an expired lease's renewal.
	WallTime(context.Context) (time.Time, error)
}

type Backend interface {
	Begin(context.Context, Isolation) (Tx, error)
	CheckReadiness(context.Context) error
	PoolStats() PoolStats
}

// Must resolves a domain store provider implemented by v (or any wrapped
// transaction exposed via Unwrap() Tx). Production transactions always
// implement every domain store.
func Must[T any](v any) T {
	for cur := v; cur != nil; {
		if s, ok := cur.(T); ok {
			return s
		}
		u, ok := cur.(interface{ Unwrap() Tx })
		if !ok {
			break
		}
		cur = u.Unwrap()
	}
	var zero T
	panic(fmt.Sprintf("database: %T does not implement %T", v, zero))
}

// Within never retries a business callback. Cleanup must work even after the
// request is cancelled, and also runs when the callback panics.
func Within(ctx context.Context, backend Backend, isolation Isolation, change func(Tx) error) error {
	tx, err := backend.Begin(ctx, isolation)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if err := change(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
