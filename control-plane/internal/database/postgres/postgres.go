// Package postgres adapts the existing pgx pool without changing its ownership,
// connection settings, migration runner, or transaction boundaries.
package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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

// WrapTx adapts owner migration transactions and explicit test fixtures. It
// borrows the exact pgx transaction, not its pool, so domain stores cannot
// accidentally commit separately.
func WrapTx(tx pgx.Tx) database.Tx { return &transaction{store{tx}, tx} }

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

func (t *transaction) WallTime(ctx context.Context) (at value.Timestamp, err error) {
	err = t.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return
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

var _ database.Diagnostics = (*Backend)(nil)

type SchedulerLeaseStore struct{ tx database.Tx }

func (s *SchedulerLeaseStore) RecordMaintenanceCompletion(ctx context.Context, owner schedulerlease.Owner, epoch int64) error {
	_, err := s.tx.Exec(ctx, `SELECT public.test_record_scheduler_maintenance($1,$2,$3)`, owner.InstanceID, owner.Incarnation, epoch)
	return err
}

func (t *transaction) SchedulerLeaseStore() schedulerlease.Store { return &SchedulerLeaseStore{tx: t} }
func (s *SchedulerLeaseStore) Lock(ctx context.Context) (schedulerlease.State, error) {
	var v schedulerlease.State
	err := s.tx.QueryRow(ctx, `SELECT instance_id,incarnation,epoch,lease_until FROM scheduler_leadership WHERE id=1 FOR UPDATE`).Scan(&v.Owner.InstanceID, &v.Owner.Incarnation, &v.Epoch, &v.Until)
	return v, err
}
func (s *SchedulerLeaseStore) Put(ctx context.Context, v schedulerlease.State, now value.Timestamp) error {
	n, err := s.tx.Exec(ctx, `UPDATE scheduler_leadership SET instance_id=$1,incarnation=$2,epoch=$3,lease_until=$4,updated_at=$5 WHERE id=1`, v.Owner.InstanceID, v.Owner.Incarnation, v.Epoch, v.Until, now)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}
func (s *SchedulerLeaseStore) Assert(ctx context.Context, owner schedulerlease.Owner, epoch int64) error {
	var until, at value.Timestamp
	err := s.tx.QueryRow(ctx, `SELECT lease_until, clock_timestamp() FROM scheduler_leadership WHERE id=1 AND instance_id=$1 AND incarnation=$2 AND epoch=$3 AND lease_until>clock_timestamp() FOR SHARE OF scheduler_leadership`, owner.InstanceID, owner.Incarnation, epoch).Scan(&until, &at)
	if errors.Is(err, database.ErrNotFound) {
		return schedulerlease.ErrLost
	}
	if err != nil {
		return err
	}
	if until.Micros <= at.Micros {
		return schedulerlease.ErrLost
	}
	return nil
}

func (t *transaction) TransactionTime(ctx context.Context) (value.Timestamp, error) {
	var now value.Timestamp
	err := t.QueryRow(ctx, "SELECT transaction_timestamp()").Scan(&now)
	return now, err
}

type invalidLogicalArgument struct{ err error }

func (a invalidLogicalArgument) Value() (driver.Value, error) { return nil, a.err }

func logicalArguments(args []any) []any {
	firstLogical := -1
	for i, arg := range args {
		switch arg.(type) {
		case value.JSONB, value.Timestamp, value.TextArray:
			firstLogical = i
		}
		if firstLogical >= 0 {
			break
		}
	}
	if firstLogical < 0 {
		return args
	}
	result := append([]any(nil), args...)
	for i := firstLogical; i < len(result); i++ {
		switch v := result[i].(type) {
		case value.JSONB:
			if !v.Valid() {
				result[i] = nil
			} else {
				result[i] = json.RawMessage(v.Bytes())
			}
		case value.Timestamp:
			if err := v.Validate(); err != nil {
				result[i] = invalidLogicalArgument{err}
				continue
			}
			p := pgtype.Timestamptz{Valid: v.Valid}
			switch v.Micros {
			case value.NegativeInfinity:
				p.InfinityModifier = pgtype.NegativeInfinity
			case value.PositiveInfinity:
				p.InfinityModifier = pgtype.Infinity
			default:
				if v.Valid {
					p.Time, _ = v.Time()
				}
			}
			result[i] = p
		case value.TextArray:
			if err := v.Validate(); err != nil {
				result[i] = invalidLogicalArgument{err}
				continue
			}
			p := pgtype.Array[pgtype.Text]{Valid: v.Valid}
			if v.Valid {
				p.Dims = make([]pgtype.ArrayDimension, len(v.Dimensions))
				p.Elements = make([]pgtype.Text, len(v.Elements))
			}
			for j, d := range v.Dimensions {
				p.Dims[j] = pgtype.ArrayDimension{Length: d.Length, LowerBound: d.LowerBound}
			}
			for j, s := range v.Elements {
				if s != nil {
					p.Elements[j] = pgtype.Text{String: *s, Valid: true}
				}
			}
			result[i] = p
		}
	}
	return result
}

const (
	logicalKindTimestamp uint8 = 1
	logicalKindJSONB     uint8 = 2
	logicalKindTextArray uint8 = 3
)

type logicalSlot struct {
	destIdx int
	kind    uint8
	valIdx  int
}

// Decode using pgx's native codecs, then publish a logical value only after the
// entire row scanned successfully. No textual array flattening or time sentinel.
func scanLogical(scan func(...any) error, dest []any) error {
	firstLogical := -1
	for i, dst := range dest {
		switch dst.(type) {
		case *value.Timestamp, *value.JSONB, *value.TextArray:
			firstLogical = i
		}
		if firstLogical >= 0 {
			break
		}
	}
	if firstLogical < 0 {
		return scan(dest...)
	}

	var argsBuf [16]any
	var args []any
	if len(dest) <= len(argsBuf) {
		args = argsBuf[:len(dest)]
	} else {
		args = make([]any, len(dest))
	}
	copy(args, dest)

	var slotsBuf [8]logicalSlot
	slots := slotsBuf[:0]
	var tsBuf [8]pgtype.Timestamptz
	var tsExtra []pgtype.Timestamptz
	tsCount := 0
	var jsonBuf [4][]byte
	var jsonExtra [][]byte
	jsonCount := 0
	var arrBuf [4]pgtype.Array[pgtype.Text]
	var arrExtra []pgtype.Array[pgtype.Text]
	arrCount := 0

	for i := firstLogical; i < len(dest); i++ {
		switch dest[i].(type) {
		case *value.Timestamp:
			if tsCount < len(tsBuf) {
				args[i] = &tsBuf[tsCount]
			} else {
				tsExtra = append(tsExtra, pgtype.Timestamptz{})
				args[i] = &tsExtra[len(tsExtra)-1]
			}
			slots = append(slots, logicalSlot{destIdx: i, kind: logicalKindTimestamp, valIdx: tsCount})
			tsCount++
		case *value.JSONB:
			if jsonCount < len(jsonBuf) {
				args[i] = &jsonBuf[jsonCount]
			} else {
				jsonExtra = append(jsonExtra, nil)
				args[i] = &jsonExtra[len(jsonExtra)-1]
			}
			slots = append(slots, logicalSlot{destIdx: i, kind: logicalKindJSONB, valIdx: jsonCount})
			jsonCount++
		case *value.TextArray:
			if arrCount < len(arrBuf) {
				args[i] = &arrBuf[arrCount]
			} else {
				arrExtra = append(arrExtra, pgtype.Array[pgtype.Text]{})
				args[i] = &arrExtra[len(arrExtra)-1]
			}
			slots = append(slots, logicalSlot{destIdx: i, kind: logicalKindTextArray, valIdx: arrCount})
			arrCount++
		}
	}
	// If any extra slice grew and reallocated, re-point args to final backing elements.
	if len(tsExtra) > 1 || len(jsonExtra) > 1 || len(arrExtra) > 1 {
		for _, slot := range slots {
			switch slot.kind {
			case logicalKindTimestamp:
				if slot.valIdx >= len(tsBuf) {
					args[slot.destIdx] = &tsExtra[slot.valIdx-len(tsBuf)]
				}
			case logicalKindJSONB:
				if slot.valIdx >= len(jsonBuf) {
					args[slot.destIdx] = &jsonExtra[slot.valIdx-len(jsonBuf)]
				}
			case logicalKindTextArray:
				if slot.valIdx >= len(arrBuf) {
					args[slot.destIdx] = &arrExtra[slot.valIdx-len(arrBuf)]
				}
			}
		}
	}

	if err := scan(args...); err != nil {
		return err
	}
	for _, slot := range slots {
		switch slot.kind {
		case logicalKindTimestamp:
			p := &tsBuf[0]
			if slot.valIdx < len(tsBuf) {
				p = &tsBuf[slot.valIdx]
			} else {
				p = &tsExtra[slot.valIdx-len(tsBuf)]
			}
			v := value.Timestamp{Valid: p.Valid}
			if p.Valid {
				switch p.InfinityModifier {
				case pgtype.NegativeInfinity:
					v.Micros = value.NegativeInfinity
				case pgtype.Infinity:
					v.Micros = value.PositiveInfinity
				default:
					var err error
					v, err = value.FromTime(p.Time)
					if err != nil {
						return err
					}
				}
			}
			*dest[slot.destIdx].(*value.Timestamp) = v
		case logicalKindJSONB:
			var raw []byte
			if slot.valIdx < len(jsonBuf) {
				raw = jsonBuf[slot.valIdx]
			} else {
				raw = jsonExtra[slot.valIdx-len(jsonBuf)]
			}
			v, err := value.ParseJSONB(raw)
			if err != nil {
				return err
			}
			*dest[slot.destIdx].(*value.JSONB) = v
		case logicalKindTextArray:
			var p *pgtype.Array[pgtype.Text]
			if slot.valIdx < len(arrBuf) {
				p = &arrBuf[slot.valIdx]
			} else {
				p = &arrExtra[slot.valIdx-len(arrBuf)]
			}
			v := value.TextArray{Valid: p.Valid}
			if p.Valid {
				v.Dimensions = make([]value.Dimension, len(p.Dims))
				v.Elements = make([]*string, len(p.Elements))
			}
			for j, d := range p.Dims {
				v.Dimensions[j] = value.Dimension{Length: d.Length, LowerBound: d.LowerBound}
			}
			for j, s := range p.Elements {
				if s.Valid {
					text := s.String
					v.Elements[j] = &text
				}
			}
			if err := v.Validate(); err != nil {
				return err
			}
			*dest[slot.destIdx].(*value.TextArray) = v
		}
	}
	return nil
}
