package connectionowner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

func AcquireBackend(ctx context.Context, backend database.Backend, nodeID, connectionID [16]byte) (*Term, error) {
	var epoch int64
	err := database.WithinRetry(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := StoreFromTransaction(tx)
		if err != nil {
			return err
		}
		epoch, err = store.Acquire(ctx, nodeID, connectionID)
		return err
	})
	if errors.Is(err, database.ErrCommitUnknown) && confirmTerm(ctx, backend, nodeID, connectionID, epoch, true) == nil {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("connectionowner: acquire node session epoch: %w", err)
	}
	return &Term{nodeID: nodeID, connectionID: connectionID, epoch: epoch}, nil
}

func ReleaseTermBackend(ctx context.Context, backend database.Backend, nodeID, connectionID [16]byte, epoch int64) error {
	err := database.WithinRetry(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := StoreFromTransaction(tx)
		if err != nil {
			return err
		}
		return store.Release(ctx, nodeID, connectionID, epoch)
	})
	if errors.Is(err, database.ErrCommitUnknown) && confirmTerm(ctx, backend, nodeID, connectionID, epoch, false) == nil {
		err = nil
	}
	if errors.Is(err, database.ErrNotFound) {
		return ErrNotOwner
	}
	return err
}

func (t *Term) ReleaseBackend(ctx context.Context, backend database.Backend) error {
	return ReleaseTermBackend(ctx, backend, t.nodeID, t.connectionID, t.epoch)
}

func confirmTerm(ctx context.Context, backend database.Backend, nodeID, connectionID [16]byte, epoch int64, active bool) error {
	check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	state, err := ReadStateBackend(check, backend, nodeID)
	if err != nil {
		return err
	}
	if state.ConnectionID != connectionID || state.Epoch != epoch || state.Active != active {
		return ErrNotOwner
	}
	return nil
}

func (t *Term) AssertTransaction(ctx context.Context, tx database.Tx) error {
	store, err := StoreFromTransaction(tx)
	if err != nil {
		return err
	}
	err = store.Assert(ctx, t.nodeID, t.connectionID, t.epoch)
	if errors.Is(err, database.ErrNotFound) {
		return ErrNotOwner
	}
	return err
}

func (t *Term) AssertCurrentBackend(ctx context.Context, backend database.Backend) error {
	return database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error { return t.AssertTransaction(ctx, tx) })
}

func ReadStateBackend(ctx context.Context, backend database.Backend, nodeID [16]byte) (state OwnerState, err error) {
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := StoreFromTransaction(tx)
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
