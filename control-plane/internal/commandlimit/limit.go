// Package commandlimit serializes command admission against the configured
// environment-wide active command limit.
package commandlimit

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const advisoryLockID int64 = 0x4f435356434d444c // "OCSVCMDL"

// Available serializes dispatch reservations and returns the number of slots
// that may be leased without exceeding the environment-wide limit.
func Available(ctx context.Context, tx pgx.Tx, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockID); err != nil {
		return 0, err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT operation.id FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running','unknown') UNION SELECT command.operation_id FROM node_command_leases lease JOIN commands command ON command.id=lease.command_id WHERE lease.leased_until>now() LIMIT $1) active`, limit).Scan(&active); err != nil {
		return 0, err
	}
	if active >= limit {
		return 0, nil
	}
	return limit - active, nil
}
