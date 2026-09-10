package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

type retryBackend struct {
	database.Backend
	begin func() database.Tx
}

func (b retryBackend) Begin(context.Context, database.Isolation) (database.Tx, error) {
	return b.begin(), nil
}

type retryTx struct {
	database.Tx
	commit, rollback func(context.Context) error
}

func (t retryTx) Commit(ctx context.Context) error   { return t.commit(ctx) }
func (t retryTx) Rollback(ctx context.Context) error { return t.rollback(ctx) }

func TestCoordinationRetryBoundaries(t *testing.T) {
	connectionLost := errors.New("connection lost")
	for _, tc := range []struct {
		name                       string
		callback, commit, rollback error
		attempts                   int
		unknown                    bool
	}{
		{"deadlock", database.ErrDeadlock, nil, connectionLost, 3, false},
		{"serialization-rolled-back", database.ErrSerialization, nil, nil, 3, false},
		{"serialization-unconfirmed", database.ErrSerialization, nil, connectionLost, 1, false},
		{"constraint", database.ErrUnique, nil, nil, 1, false},
		{"statement-disconnect", connectionLost, nil, nil, 1, false},
		{"unknown-commit", nil, connectionLost, database.ErrTxClosed, 1, true},
		{"cancelled-commit", nil, context.Canceled, database.ErrTxClosed, 1, true},
		{"aborted-commit", nil, database.ErrTxAborted, database.ErrTxClosed, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, cleanups := 0, 0
			b := retryBackend{begin: func() database.Tx {
				return retryTx{commit: func(context.Context) error { return tc.commit }, rollback: func(ctx context.Context) error {
					cleanups++
					if ctx.Err() != nil {
						t.Error("cancelled cleanup")
					}
					return tc.rollback
				}}
			}}
			err := database.WithinRetry(context.Background(), b, database.ReadCommitted, func(database.Tx) error {
				calls++
				return tc.callback
			})
			if err == nil || calls != tc.attempts || cleanups != tc.attempts || errors.Is(err, database.ErrCommitUnknown) != tc.unknown {
				t.Fatalf("calls=%d cleanups=%d error=%v", calls, cleanups, err)
			}
		})
	}
	t.Run("successful-retry", func(t *testing.T) {
		calls := 0
		b := retryBackend{begin: func() database.Tx {
			return retryTx{commit: func(context.Context) error { return nil }, rollback: func(context.Context) error { return nil }}
		}}
		if err := database.WithinRetry(context.Background(), b, database.ReadCommitted, func(database.Tx) error {
			calls++
			if calls == 1 {
				return database.ErrDeadlock
			}
			return nil
		}); err != nil || calls != 2 {
			t.Fatal(calls, err)
		}
	})
	t.Run("cancellation-stops-retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		b := retryBackend{begin: func() database.Tx {
			return retryTx{rollback: func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Error("cleanup inherited cancellation")
				}
				return nil
			}}
		}}
		err := database.WithinRetry(ctx, b, database.ReadCommitted, func(database.Tx) error {
			calls++
			cancel()
			return database.ErrDeadlock
		})
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatal(calls, err)
		}
	})
}
