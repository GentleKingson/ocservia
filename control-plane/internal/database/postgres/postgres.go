// Package postgres adapts the existing pgx pool without changing its ownership,
// connection settings, migration runner, or transaction boundaries.
package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

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

func (b *Backend) Pool() *pgxpool.Pool { return b.pool }

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
	return &transaction{store{tx}, tx}, nil
}

func (s store) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := s.executor.Exec(ctx, sql, args...)
	return tag.RowsAffected(), classify(err)
}

type Row struct{ pgx.Row }

func (r Row) Scan(dest ...any) error { return classify(r.Row.Scan(dest...)) }
func (s store) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return Row{s.executor.QueryRow(ctx, sql, args...)}
}

type Rows struct{ pgx.Rows }

func (r Rows) Scan(dest ...any) error { return classify(r.Rows.Scan(dest...)) }
func (r Rows) Err() error             { return classify(r.Rows.Err()) }
func (s store) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	r, err := s.executor.Query(ctx, sql, args...)
	if err != nil {
		return Rows{}, classify(err)
	}
	return Rows{r}, nil
}

func walkRows(ctx context.Context, s store, query string, visit func(Rows) error, args ...any) error {
	r, err := s.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer r.Close()
	for r.Next() {
		if err := visit(r); err != nil {
			return err
		}
	}
	return r.Err()
}

func collectRows[T any](ctx context.Context, s store, query string, scan func(Rows) (T, error), args ...any) ([]T, error) {
	out := make([]T, 0)
	err := walkRows(ctx, s, query, func(r Rows) error {
		v, err := scan(r)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	}, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func collectColumn[T any](ctx context.Context, s store, query string, args ...any) ([]T, error) {
	return collectRows(ctx, s, query, func(r Rows) (T, error) {
		var v T
		err := r.Scan(&v)
		return v, err
	}, args...)
}

func wallTime(ctx context.Context, s store) (at time.Time, err error) {
	err = s.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return at.UTC(), err
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
			case "08003", "08006", "57P01", "57P02", "57P03":
				kind = database.ErrUnavailable
			}
		} else {
			var network net.Error
			if errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				kind = database.ErrUnavailable
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

func (t *transaction) WallTime(ctx context.Context) (time.Time, error) {
	return wallTime(ctx, t.store)
}

func (b *Backend) CheckReadiness(ctx context.Context) error {
	// Check current core reads and permissions without scanning migration history.
	var workspace, node, operation bool
	return b.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE false), EXISTS(SELECT 1 FROM nodes WHERE false), EXISTS(SELECT 1 FROM operations WHERE false)`).Scan(&workspace, &node, &operation)
}

func (b *Backend) PoolStats() database.PoolStats {
	s := b.pool.Stat()
	return database.PoolStats{Acquired: int64(s.AcquiredConns()), Idle: int64(s.IdleConns()), Total: int64(s.TotalConns())}
}
