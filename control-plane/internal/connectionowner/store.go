package connectionowner

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Identity struct {
	InstanceID  uuid.UUID
	Incarnation int64
}

type StoreTerm struct {
	Identity             Identity
	NodeID, ConnectionID [16]byte
	Epoch                int64
}

type Lease struct {
	Epoch int64
	Until value.Timestamp
}

type OwnerState struct {
	InstanceID      uuid.UUID
	Incarnation     int64
	ConnectionID    [16]byte
	Epoch           int64
	Until           value.Timestamp
	LeaseUntilValid bool
}

type Store interface {
	Acquire(context.Context, StoreTerm, time.Duration) (Lease, error)
	Renew(context.Context, StoreTerm, time.Duration) (value.Timestamp, error)
	Release(context.Context, StoreTerm) error
	Assert(context.Context, StoreTerm) error
	Read(context.Context, [16]byte) (OwnerState, error)
}

func StoreFromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ ConnectionOwnerStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.ConnectionOwnerStore(), nil
}
