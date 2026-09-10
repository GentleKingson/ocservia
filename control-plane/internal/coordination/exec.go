package coordination

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AssertFenceTx does not commit: database.Within remains the transaction owner.
func AssertFenceTx(ctx context.Context, tx database.Tx, fence Fence) error {
	if fence == nil {
		return nil
	}
	generic, ok := fence.(interface {
		AssertTransaction(context.Context, database.Tx) error
	})
	if !ok {
		return database.ErrUnsupported
	}
	return generic.AssertTransaction(ctx, tx)
}

// CommitFenced asserts leadership inside the transaction immediately before
// commit. A nil fence preserves the legacy unfenced path used by direct
// service tests; production scheduler wiring always passes a real fence.
func CommitFenced(ctx context.Context, tx pgx.Tx, fence Fence) error {
	if fence != nil {
		if err := fence.AssertLeader(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// FencedExec executes a single statement in its own transaction and asserts
// leadership before commit, so the statement cannot land after a takeover.
// A nil fence falls back to an unfenced autocommit execution.
func FencedExec(ctx context.Context, pool *pgxpool.Pool, fence Fence, sql string, args ...any) error {
	if fence == nil {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return CommitFenced(ctx, tx, fence)
}
