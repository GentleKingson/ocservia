// Package database defines the driver-free Controller database boundary.
package database

import (
	"context"
	"errors"
	"time"
)

type Isolation uint8

const (
	DefaultIsolation Isolation = iota
	ReadCommitted
	RepeatableRead
	Serializable
)

type Capability string

const (
	Transactions             Capability = "transactions"
	RowLocks                 Capability = "row_locks"
	AdvisoryTransactionLocks Capability = "advisory_transaction_locks"
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
)

type Row interface{ Scan(...any) error }
type Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

// Store executes backend-owned SQL. Business modules use domain-specific
// stores instead; SQL dialects are not translated by this interface.
type Store interface {
	Exec(context.Context, string, ...any) (int64, error)
	QueryRow(context.Context, string, ...any) Row
	Query(context.Context, string, ...any) (Rows, error)
}

// Tx is shared by every store participating in one business operation.
// Only the transaction owner commits or rolls back; stores never begin a Tx.
type Tx interface {
	Store
	Commit(context.Context) error
	Rollback(context.Context) error
}

type Backend interface {
	Store
	Begin(context.Context, Isolation) (Tx, error)
	Supports(Capability) bool
}

func Require(backend Backend, capabilities ...Capability) error {
	for _, capability := range capabilities {
		if !backend.Supports(capability) {
			return ErrUnsupported
		}
	}
	return nil
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
