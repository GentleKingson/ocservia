// Package postgres adapts the existing pgx pool without changing its ownership,
// connection settings, migration runner, or transaction boundaries.
package postgres

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
type store struct{ executor executor }
type Backend struct {
	store
	pool *pgxpool.Pool
}
type transaction struct {
	store
	tx pgx.Tx
}

func WrapPool(pool *pgxpool.Pool) *Backend { return &Backend{store{pool}, pool} }

// WrapTx is the temporary legacy bridge. It borrows the exact pgx transaction,
// not its pool, so migrated stores cannot accidentally commit separately.
func WrapTx(tx pgx.Tx) database.Tx { return &transaction{store{tx}, tx} }

func (b *Backend) Supports(c database.Capability) bool {
	switch c {
	case database.Transactions, database.RowLocks, database.AdvisoryTransactionLocks:
		return true
	default:
		return false
	}
}

func (b *Backend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	var level pgx.TxIsoLevel
	switch isolation {
	case database.DefaultIsolation:
	case database.ReadCommitted:
		level = pgx.ReadCommitted
	case database.RepeatableRead:
		level = pgx.RepeatableRead
	case database.Serializable:
		level = pgx.Serializable
	default:
		return nil, database.ErrUnsupported
	}
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
	if err != nil {
		return nil, classify(err)
	}
	return WrapTx(tx), nil
}
func (s store) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := s.executor.Exec(ctx, sql, logicalArguments(args)...)
	return tag.RowsAffected(), classify(err)
}

type row struct{ pgx.Row }

func (r row) Scan(dest ...any) error { return classify(scanLogical(r.Row.Scan, dest)) }
func (s store) QueryRow(ctx context.Context, sql string, args ...any) database.Row {
	return row{s.executor.QueryRow(ctx, sql, logicalArguments(args)...)}
}

type rows struct{ pgx.Rows }

func (r rows) Scan(dest ...any) error { return classify(scanLogical(r.Rows.Scan, dest)) }
func (r rows) Err() error             { return classify(r.Rows.Err()) }
func (s store) Query(ctx context.Context, sql string, args ...any) (database.Rows, error) {
	r, err := s.executor.Query(ctx, sql, logicalArguments(args)...)
	if err != nil {
		return nil, classify(err)
	}
	return rows{r}, nil
}
func (t *transaction) Commit(ctx context.Context) error   { return classify(t.tx.Commit(ctx)) }
func (t *transaction) Rollback(ctx context.Context) error { return classify(t.tx.Rollback(ctx)) }

// Keep the original error chain for existing callers during migration. New
// business code uses errors.Is with database sentinels, never SQLSTATE/pgconn.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var kind error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, pgx.ErrNoRows):
		kind = database.ErrNotFound
	case errors.Is(err, pgx.ErrTxClosed):
		kind = database.ErrTxClosed
	case errors.Is(err, pgx.ErrTxCommitRollback):
		kind = database.ErrTxAborted
	default:
		var pg *pgconn.PgError
		if errors.As(err, &pg) {
			switch pg.Code {
			case "23505":
				kind = database.ErrUnique
			case "23503":
				kind = database.ErrForeignKey
			case "23502", "23514", "23P01":
				kind = database.ErrConstraint
			case "40001":
				kind = database.ErrSerialization
			case "40P01":
				kind = database.ErrDeadlock
			case "42501":
				kind = database.ErrPermission
			case "25P02":
				kind = database.ErrTxAborted
			}
		}
	}
	if kind == nil {
		return err
	}
	return errors.Join(kind, err)
}

var _ database.Backend = (*Backend)(nil)
var _ database.Tx = (*transaction)(nil)
