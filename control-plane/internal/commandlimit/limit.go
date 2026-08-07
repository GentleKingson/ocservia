// Package commandlimit serializes command admission against the configured
// environment-wide active command limit.
package commandlimit

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var ErrExceeded = errors.New("global active command limit reached")

const advisoryLockID int64 = 0x4f435356434d444c // "OCSVCMDL"

// Reserve must run in the transaction that creates the command.
func Reserve(ctx context.Context, tx pgx.Tx, limit int) error {
	if limit < 1 {
		return ErrExceeded
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockID); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('queued','dispatched','accepted','running','offline_pending','unknown')`).Scan(&active); err != nil {
		return err
	}
	if active >= limit {
		return ErrExceeded
	}
	return nil
}
