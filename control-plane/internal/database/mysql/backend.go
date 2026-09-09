package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type store struct{ executor executor }
type Backend struct {
	store
	pool   *sql.DB
	engine Engine
}
type transaction struct {
	store
	tx *sql.Tx
}

func (b *Backend) Close() error { return safeError(b.pool.Close()) }
func (b *Backend) Supports(c database.Capability) bool {
	return c == database.Transactions || c == database.RowLocks
}
func (b *Backend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	var level sql.IsolationLevel
	switch isolation {
	case database.DefaultIsolation, database.ReadCommitted:
		level = sql.LevelReadCommitted
	case database.RepeatableRead:
		level = sql.LevelRepeatableRead
	case database.Serializable:
		level = sql.LevelSerializable
	default:
		return nil, database.ErrUnsupported
	}
	tx, err := b.pool.BeginTx(ctx, &sql.TxOptions{Isolation: level})
	if err != nil {
		return nil, safeError(err)
	}
	return &transaction{store{tx}, tx}, nil
}
func (s store) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	r, err := s.executor.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, safeError(err)
	}
	n, err := r.RowsAffected()
	return n, safeError(err)
}

type row struct{ *sql.Row }

func (r row) Scan(dest ...any) error { return safeError(r.Row.Scan(dest...)) }

type rows struct{ *sql.Rows }

func (r rows) Scan(dest ...any) error { return safeError(r.Rows.Scan(dest...)) }
func (r rows) Err() error             { return safeError(r.Rows.Err()) }
func (r rows) Close()                 { _ = r.Rows.Close() }
func (s store) QueryRow(ctx context.Context, query string, args ...any) database.Row {
	return row{s.executor.QueryRowContext(ctx, query, args...)}
}
func (s store) Query(ctx context.Context, query string, args ...any) (database.Rows, error) {
	r, err := s.executor.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, safeError(err)
	}
	return rows{r}, nil
}
func (t *transaction) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		_ = t.tx.Rollback()
		return err
	}
	return safeError(t.tx.Commit())
}
func (t *transaction) Rollback(context.Context) error { return safeError(t.tx.Rollback()) }

func classifyNumber(code uint16) error {
	switch code {
	case 1062:
		return database.ErrUnique
	case 1451, 1452:
		return database.ErrForeignKey
	case 1048, 3819, 4025:
		return database.ErrConstraint
	case 1213:
		return database.ErrDeadlock
	case 1044, 1045, 1142, 1143, 1227, 1370:
		return database.ErrPermission
	default:
		return fmt.Errorf("experimental database: server error %d (details redacted)", code)
	}
}

// LockTransaction uses the caller's transaction, never a session GET_LOCK.
// Keys retain the PostgreSQL logical namespace and are acquired at the same
// call sites/order when domain stores are ported. No automatic sorting/retry.
func LockTransaction(ctx context.Context, tx database.Tx, key string) error {
	t, ok := tx.(*transaction)
	if !ok || len(key) == 0 || len(key) > 128 {
		return database.ErrUnsupported
	}
	_, err := t.Exec(ctx, "INSERT INTO business_locks(lock_key) VALUES (?) ON DUPLICATE KEY UPDATE lock_key=lock_key", []byte(key))
	if err != nil {
		return err
	}
	var locked []byte
	return t.QueryRow(ctx, "SELECT lock_key FROM business_locks WHERE lock_key=? FOR UPDATE", []byte(key)).Scan(&locked)
}

var _ database.Backend = (*Backend)(nil)
var _ database.Tx = (*transaction)(nil)
