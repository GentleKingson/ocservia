package ownerstore

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

type Term struct {
	Identity             Identity
	NodeID, ConnectionID [16]byte
	Epoch                int64
}

type Lease struct {
	Epoch int64
	Until value.Timestamp
}

type State struct {
	InstanceID      uuid.UUID
	Incarnation     int64
	ConnectionID    [16]byte
	Epoch           int64
	LeaseUntilValid bool
}

type Store interface {
	Acquire(context.Context, Term, time.Duration) (Lease, error)
	Renew(context.Context, Term, time.Duration) (value.Timestamp, error)
	Release(context.Context, Term) error
	Assert(context.Context, Term) error
	Read(context.Context, [16]byte) (State, error)
}

func FromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ ConnectionOwnerStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.ConnectionOwnerStore(), nil
}
