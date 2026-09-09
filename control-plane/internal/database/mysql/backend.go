package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

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
	conn   *sql.Conn
	tx     *sql.Tx
	abort  func()
	cancel context.CancelFunc
	done   atomic.Bool
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
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		return nil, safeError(err)
	}
	var abort func()
	if err = conn.Raw(func(raw any) error {
		physical, ok := raw.(*physicalConnection)
		if !ok {
			return database.ErrUnsupported
		}
		abort = physical.abort
		return nil
	}); err != nil {
		_ = conn.Close()
		return nil, safeError(err)
	}
	// Detach only the transaction lifetime, not Begin's cancellation. Otherwise
	// database/sql starts an unbounded background rollback on request cancellation
	// before Within can supply its independent cleanup context.
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	tx, err := conn.BeginTx(lifetime, &sql.TxOptions{Isolation: level})
	stop()
	if err != nil || ctx.Err() != nil {
		abort()
		cancel()
		if tx != nil {
			_ = tx.Rollback()
		}
		discard(conn)
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, safeError(err)
	}
	return &transaction{store: store{tx}, conn: conn, tx: tx, abort: abort, cancel: cancel}, nil
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
	return t.finish(ctx, t.tx.Commit)
}
func (t *transaction) Rollback(ctx context.Context) error { return t.finish(ctx, t.tx.Rollback) }

func (t *transaction) finish(ctx context.Context, finish func() error) error {
	if !t.done.CompareAndSwap(false, true) {
		return database.ErrTxClosed
	}
	defer t.cancel()
	// Close the raw socket, not Conn.Raw/Close: those can wait behind the very
	// driver operation being interrupted. Do not detach a live Commit/Rollback
	// goroutine or return a session to the pool while a watchdog can still fire.
	aborted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { t.abort(); close(aborted) })
	var err error
	if ctx.Err() != nil {
		t.abort()
		_ = t.tx.Rollback()
		err = ctx.Err()
	} else {
		err = finish()
	}
	if !stop() {
		<-aborted
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		t.abort()
		discard(t.conn)
	}
	closeErr := t.conn.Close()
	if err != nil {
		return safeError(err)
	}
	return safeError(closeErr)
}

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
