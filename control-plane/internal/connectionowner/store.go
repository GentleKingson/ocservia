package connectionowner

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

type OwnerState struct {
	ConnectionID [16]byte
	Epoch        int64
	Active       bool
}

type Store interface {
	Acquire(ctx context.Context, nodeID, connectionID [16]byte) (int64, error)
	Release(ctx context.Context, nodeID, connectionID [16]byte, epoch int64) error
	Assert(ctx context.Context, nodeID, connectionID [16]byte, epoch int64) error
	Read(ctx context.Context, nodeID [16]byte) (OwnerState, error)
}

func StoreFromTransaction(tx database.Tx) Store {
	return database.Must[interface{ ConnectionOwnerStore() Store }](tx).ConnectionOwnerStore()
}
