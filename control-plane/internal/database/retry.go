package database

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

var ErrCommitUnknown = errors.New("database: commit outcome unknown")

// WithinRetry is opt-in for database-only coordination work. The callback must
// reset attempt-local results and must never perform transport or other external
// mutations. An unknown commit is returned for domain-specific record readback,
// never replayed. Within intentionally retains its no-retry contract.
func WithinRetry(ctx context.Context, backend Backend, isolation Isolation, change func(Tx) error) error {
	for attempt := 0; ; attempt++ {
		err, retry := retryAttempt(ctx, backend, isolation, change)
		if !retry || attempt == 2 {
			return err
		}
		delay := 10 * time.Millisecond << attempt
		timer := time.NewTimer(delay + rand.N(delay))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func retryAttempt(ctx context.Context, backend Backend, isolation Isolation, change func(Tx) error) (err error, retry bool) {
	tx, err := backend.Begin(ctx, isolation)
	if err != nil {
		return err, false
	}
	committing := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rollback := tx.Rollback(cleanup)
		// PostgreSQL aborts a deadlock victim's entire transaction; other
		// serialization failures require an acknowledged rollback before retry.
		retry = errors.Is(err, ErrDeadlock) || errors.Is(err, ErrSerialization) && rollback == nil
		if committing && err != nil && !retry && !errors.Is(err, ErrTxAborted) {
			err = errors.Join(ErrCommitUnknown, err)
		}
	}()
	if err = change(tx); err != nil {
		return err, false
	}
	committing = true
	return tx.Commit(ctx), false
}
