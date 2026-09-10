package mysql

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

// InnoDB can roll back the entire server transaction while sql.Tx is still
// open. Never let a caught deadlock/snapshot error turn later writes into
// autocommit statements. Closing the socket also prevents accidental reuse.
func (t *transaction) queryError(err error) error {
	if errors.Is(err, database.ErrDeadlock) || errors.Is(err, database.ErrSerialization) {
		t.poisoned.Store(true)
		t.abort()
	}
	return err
}

func (t *transaction) queryReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.done.Load() {
		return database.ErrTxClosed
	}
	if t.poisoned.Load() {
		return database.ErrTxAborted
	}
	return nil
}

func (t *transaction) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if err := t.queryReady(ctx); err != nil {
		return 0, err
	}
	n, err := t.store.Exec(ctx, query, args...)
	return n, t.queryError(err)
}

type transactionRow struct {
	row database.Row
	tx  *transaction
	err error
}

func (r transactionRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.tx.queryError(r.row.Scan(dest...))
}
func (t *transaction) QueryRow(ctx context.Context, query string, args ...any) database.Row {
	if err := t.queryReady(ctx); err != nil {
		return transactionRow{err: err}
	}
	r := t.store.executor.QueryRowContext(ctx, query, logicalArguments(args)...)
	// QueryRow executes immediately but defers its error until Scan. Poison
	// before returning, even if the caller never scans this row.
	if err := t.queryError(safeError(r.Err())); err != nil {
		return transactionRow{err: err}
	}
	return transactionRow{row: row{r}, tx: t}
}

type transactionRows struct {
	database.Rows
	tx *transaction
}

func (r transactionRows) Scan(dest ...any) error { return r.tx.queryError(r.Rows.Scan(dest...)) }
func (r transactionRows) Err() error             { return r.tx.queryError(r.Rows.Err()) }
func (r transactionRows) Close() {
	// Closing unread rows drains result packets, which can reveal a server
	// rollback after the initial result-set header was returned successfully.
	r.Rows.Close()
	r.tx.queryError(r.Rows.Err())
}
func (r transactionRows) Next() bool {
	if r.tx.poisoned.Load() {
		r.Rows.Close()
		return false
	}
	ok := r.Rows.Next()
	if !ok {
		r.tx.queryError(r.Rows.Err())
	}
	return ok
}
func (t *transaction) Query(ctx context.Context, query string, args ...any) (database.Rows, error) {
	if err := t.queryReady(ctx); err != nil {
		return nil, err
	}
	r, err := t.store.Query(ctx, query, args...)
	if err != nil {
		return nil, t.queryError(err)
	}
	return transactionRows{Rows: r, tx: t}, nil
}
