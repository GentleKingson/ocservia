// Package commandlimit serializes command admission against the configured
// environment-wide active command limit.
package commandlimit

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var ErrBacklogExceeded = errors.New("remote command backlog limit reached")

const (
	AdmissionLockID     int64 = 0x4f435356434d444c // "OCSVCMDL"
	BacklogLockID       int64 = 0x4f4353564241434b // "OCSVBACK"
	MaxNodeBacklog            = 500
	MaxWorkspaceBacklog       = 5000
)

type Store interface {
	LockAdmission(context.Context) error
	LockBacklog(context.Context) error
	Active(context.Context, int) (int, error)
	NodeBacklog(context.Context, uuid.UUID, int) (int, error)
	WorkspaceBacklog(context.Context, uuid.UUID, int) (int, error)
}

func storeFor(tx database.Tx) Store {
	return database.Must[interface{ CommandLimitStore() Store }](tx).CommandLimitStore()
}

func Lock(ctx context.Context, tx database.Tx) error {
	return storeFor(tx).LockAdmission(ctx)
}

// Available serializes dispatch reservations and returns the number of slots
// that may be leased without exceeding the environment-wide limit.
func Available(ctx context.Context, tx database.Tx, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	store := storeFor(tx)
	if err := store.LockAdmission(ctx); err != nil {
		return 0, err
	}
	active, err := store.Active(ctx, limit)
	if err != nil {
		return 0, err
	}
	if active >= limit {
		return 0, nil
	}
	return limit - active, nil
}

// ReserveBacklog bounds durable queued work independently from execution
// concurrency so offline nodes cannot consume dispatch slots or unbounded DB.
func ReserveBacklog(ctx context.Context, tx database.Tx, workspaceID, nodeID uuid.UUID) error {
	store := storeFor(tx)
	if err := store.LockBacklog(ctx); err != nil {
		return err
	}
	nodeCount, err := store.NodeBacklog(ctx, nodeID, MaxNodeBacklog)
	if err != nil {
		return err
	}
	if nodeCount >= MaxNodeBacklog {
		return ErrBacklogExceeded
	}
	workspaceCount, err := store.WorkspaceBacklog(ctx, workspaceID, MaxWorkspaceBacklog)
	if err != nil {
		return err
	}
	if workspaceCount >= MaxWorkspaceBacklog {
		return ErrBacklogExceeded
	}
	return nil
}
