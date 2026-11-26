package connectionowner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

func Acquire(ctx context.Context, backend database.Backend, nodeID, connectionID [16]byte) (*Term, error) {
	var epoch int64
	err := database.WithinRetry(ctx, backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		epoch, err = StoreFromTransaction(tx).Acquire(ctx, nodeID, connectionID)
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

func ReleaseTerm(ctx context.Context, backend database.Backend, nodeID, connectionID [16]byte, epoch int64) error {
	err := database.WithinRetry(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		return StoreFromTransaction(tx).Release(ctx, nodeID, connectionID, epoch)
	})
	if errors.Is(err, database.ErrCommitUnknown) && confirmTerm(ctx, backend, nodeID, connectionID, epoch, false) == nil {
		err = nil
	}
	if errors.Is(err, database.ErrNotFound) {
		return ErrNotOwner
	}
	return err
}

func (t *Term) Release(ctx context.Context, backend database.Backend) error {
	return ReleaseTerm(ctx, backend, t.nodeID, t.connectionID, t.epoch)
}

func confirmTerm(ctx context.Context, backend database.Backend, nodeID, connectionID [16]byte, epoch int64, active bool) error {
	check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	state, err := ReadState(check, backend, nodeID)
	if err != nil {
		return err
	}
	if state.ConnectionID != connectionID || state.Epoch != epoch || state.Active != active {
		return ErrNotOwner
	}
	return nil
}

func (t *Term) AssertTransaction(ctx context.Context, tx database.Tx) error {
	err := StoreFromTransaction(tx).Assert(ctx, t.nodeID, t.connectionID, t.epoch)
	if errors.Is(err, database.ErrNotFound) {
		return ErrNotOwner
	}
	return err
}

func (t *Term) AssertCurrent(ctx context.Context, backend database.Backend) error {
	return database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error { return t.AssertTransaction(ctx, tx) })
}

func ReadState(ctx context.Context, backend database.Backend, nodeID [16]byte) (state OwnerState, err error) {
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		state, err = StoreFromTransaction(tx).Read(ctx, nodeID)
		return err
	})
	if errors.Is(err, database.ErrNotFound) {
		return OwnerState{}, fmt.Errorf("%w: node %x", ErrNoOwnerRow, nodeID)
	}
	return
}
