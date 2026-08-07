// Package commandlimit serializes command admission against the configured
// environment-wide active command limit.
package commandlimit

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrBacklogExceeded = errors.New("remote command backlog limit reached")

const (
	advisoryLockID        int64 = 0x4f435356434d444c // "OCSVCMDL"
	backlogAdvisoryLockID int64 = 0x4f4353564241434b // "OCSVBACK"
	MaxNodeBacklog              = 500
	MaxWorkspaceBacklog         = 5000
)

func Lock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockID)
	return err
}

// Available serializes dispatch reservations and returns the number of slots
// that may be leased without exceeding the environment-wide limit.
func Available(ctx context.Context, tx pgx.Tx, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	if err := Lock(ctx, tx); err != nil {
		return 0, err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT operation.id FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running','unknown') UNION SELECT command.operation_id FROM node_command_leases lease JOIN commands command ON command.id=lease.command_id LIMIT $1) active`, limit).Scan(&active); err != nil {
		return 0, err
	}
	if active >= limit {
		return 0, nil
	}
	return limit - active, nil
}

// ReserveBacklog bounds durable queued work independently from execution
// concurrency so offline nodes cannot consume dispatch slots or unbounded DB.
func ReserveBacklog(ctx context.Context, tx pgx.Tx, workspaceID, nodeID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, backlogAdvisoryLockID); err != nil {
		return err
	}
	var nodeCount, workspaceCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM operations WHERE node_id=$1 AND state IN('queued','offline_pending') LIMIT $2) backlog`, nodeID, MaxNodeBacklog).Scan(&nodeCount); err != nil {
		return err
	}
	if nodeCount >= MaxNodeBacklog {
		return ErrBacklogExceeded
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM operations WHERE workspace_id=$1 AND state IN('queued','offline_pending') LIMIT $2) backlog`, workspaceID, MaxWorkspaceBacklog).Scan(&workspaceCount); err != nil {
		return err
	}
	if workspaceCount >= MaxWorkspaceBacklog {
		return ErrBacklogExceeded
	}
	return nil
}
