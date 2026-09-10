package connectionowner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func AcquireBackend(ctx context.Context, backend database.Backend, nodeID [16]byte, identity Identity, connectionID [16]byte, leaseTTL time.Duration) (*Term, error) {
	if leaseTTL <= 0 {
		return nil, errors.New("connectionowner: lease TTL must be positive")
	}
	var lease ownerstore.Lease
	err := database.WithinRetry(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := ownerstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		lease, err = store.Acquire(ctx, ownerstore.Term{Identity: identity, NodeID: nodeID, ConnectionID: connectionID}, leaseTTL)
		return err
	})
	if errors.Is(err, database.ErrCommitUnknown) {
		term := ownerstore.Term{Identity: identity, NodeID: nodeID, ConnectionID: connectionID, Epoch: lease.Epoch}
		if confirmTerm(ctx, backend, term, lease.Until, true) == nil {
			err = nil
		}
	}
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrLeaseHeld
	}
	if err != nil {
		return nil, fmt.Errorf("connectionowner: acquire node ownership: %w", err)
	}
	until, err := lease.Until.Time()
	if err != nil {
		return nil, err
	}
	return &Term{identity: identity, nodeID: nodeID, connectionID: connectionID, epoch: lease.Epoch, leaseTTL: leaseTTL, leaseUntil: until}, nil
}

func (t *Term) storedTerm() ownerstore.Term {
	return ownerstore.Term{Identity: t.identity, NodeID: t.nodeID, ConnectionID: t.connectionID, Epoch: t.epoch}
}

func (t *Term) RenewBackend(ctx context.Context, backend database.Backend) (time.Time, error) {
	var until value.Timestamp
	err := database.WithinRetry(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := ownerstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		until, err = store.Renew(ctx, t.storedTerm(), t.leaseTTL)
		return err
	})
	if errors.Is(err, database.ErrCommitUnknown) && confirmTerm(ctx, backend, t.storedTerm(), until, true) == nil {
		err = nil
	}
	if errors.Is(err, database.ErrNotFound) {
		return time.Time{}, ErrNotOwner
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("connectionowner: renew node ownership: %w", err)
	}
	return until.Time()
}

func (t *Term) ReleaseBackend(ctx context.Context, backend database.Backend) error {
	err := database.WithinRetry(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := ownerstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.Release(ctx, t.storedTerm())
	})
	if errors.Is(err, database.ErrCommitUnknown) && confirmTerm(ctx, backend, t.storedTerm(), value.Timestamp{}, false) == nil {
		err = nil
	}
	if errors.Is(err, database.ErrNotFound) {
		return ErrNotOwner
	}
	return err
}

func confirmTerm(ctx context.Context, backend database.Backend, term ownerstore.Term, until value.Timestamp, live bool) error {
	check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	state, err := ReadStateBackend(check, backend, term.NodeID)
	if err != nil {
		return err
	}
	if state.InstanceID != term.Identity.InstanceID || state.Incarnation != term.Identity.Incarnation || state.ConnectionID != term.ConnectionID || state.Epoch != term.Epoch || state.LeaseUntilValid != live || (live && (!until.Valid || state.Until.Micros < until.Micros)) {
		return ErrNotOwner
	}
	return nil
}

func (t *Term) AssertTransaction(ctx context.Context, tx database.Tx) error {
	store, err := ownerstore.FromTransaction(tx)
	if err != nil {
		return err
	}
	err = store.Assert(ctx, t.storedTerm())
	if errors.Is(err, database.ErrNotFound) {
		return ErrNotOwner
	}
	return err
}

func (t *Term) AssertCurrentBackend(ctx context.Context, backend database.Backend) error {
	return database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error { return t.AssertTransaction(ctx, tx) })
}

// GuardObservedTermBackend keeps the transaction open for the bounded external
// mutation. Its release never depends on the request still being uncancelled.
func GuardObservedTermBackend(ctx context.Context, backend database.Backend, nodeID [16]byte, instanceID uuid.UUID, incarnation int64, connectionID [16]byte, epoch int64) (release func() error, err error) {
	tx, err := backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return nil, err
	}
	cleanup := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := tx.Rollback(ctx)
		if errors.Is(err, database.ErrTxClosed) {
			return nil
		}
		return err
	}
	defer func() {
		if err != nil {
			_ = cleanup()
		}
	}()
	store, err := ownerstore.FromTransaction(tx)
	if err != nil {
		return nil, err
	}
	err = store.Assert(ctx, ownerstore.Term{Identity: Identity{InstanceID: instanceID, Incarnation: incarnation}, NodeID: nodeID, ConnectionID: connectionID, Epoch: epoch})
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrNotOwner
	}
	if err != nil {
		return nil, err
	}
	return cleanup, nil
}

func ReadStateBackend(ctx context.Context, backend database.Backend, nodeID [16]byte) (state OwnerState, err error) {
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := ownerstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		state, err = store.Read(ctx, nodeID)
		return err
	})
	if errors.Is(err, database.ErrNotFound) {
		return OwnerState{}, fmt.Errorf("%w: node %x", ErrNoOwnerRow, nodeID)
	}
	return
}
