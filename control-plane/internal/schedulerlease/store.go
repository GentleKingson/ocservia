// Package schedulerlease owns the driver-free fenced leadership transaction.
package schedulerlease

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

var ErrHeld = errors.New("coordination: scheduler lease held by another leader")
var ErrLost = errors.New("coordination: scheduler leadership lost")

type Owner struct {
	InstanceID  uuid.UUID
	Incarnation int64
}
type State struct {
	Owner Owner
	Epoch int64
	Until value.Timestamp
}
type Store interface {
	Lock(context.Context) (State, error)
	Put(context.Context, State, value.Timestamp) error
	Assert(context.Context, Owner, int64) error
	RecordMaintenanceCompletion(context.Context, Owner, int64) error
}

func FromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ SchedulerLeaseStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.SchedulerLeaseStore(), nil
}

func deadline(now value.Timestamp, ttl time.Duration) (value.Timestamp, error) {
	if ttl <= 0 || ttl%time.Microsecond != 0 {
		return value.Timestamp{}, errors.New("coordination: lease TTL must be positive whole microseconds")
	}
	if !now.Valid || now.Micros < value.MinTimestamp || now.Micros >= value.EndTimestamp {
		return value.Timestamp{}, errors.New("coordination: non-finite transaction clock")
	}
	delta := ttl.Microseconds()
	if now.Micros > value.EndTimestamp-1-delta {
		return value.Timestamp{}, errors.New("coordination: lease deadline out of range")
	}
	return value.Timestamp{Valid: true, Micros: now.Micros + delta}, nil
}

func Acquire(ctx context.Context, b database.Backend, owner Owner, ttl time.Duration) (int64, error) {
	var epoch int64
	var next State
	err := database.WithinRetry(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		s, err := FromTransaction(tx)
		if err != nil {
			return err
		}
		prior, err := s.Lock(ctx)
		if err != nil {
			return err
		}
		now, err := database.WallTime(ctx, tx)
		if err != nil {
			return err
		}
		until, err := deadline(now, ttl)
		if err != nil {
			return err
		}
		if prior.Until.Micros > now.Micros && prior.Owner != owner {
			return ErrHeld
		}
		if prior.Epoch == math.MaxInt64 {
			return errors.New("coordination: fencing epoch exhausted")
		}
		epoch = prior.Epoch + 1
		next = State{Owner: owner, Epoch: epoch, Until: until}
		return s.Put(ctx, next, now)
	})
	if errors.Is(err, database.ErrCommitUnknown) && confirmState(ctx, b, next) == nil {
		err = nil
	}
	return epoch, err
}

func Renew(ctx context.Context, b database.Backend, owner Owner, epoch int64, ttl time.Duration) error {
	var next State
	err := database.WithinRetry(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		s, err := FromTransaction(tx)
		if err != nil {
			return err
		}
		prior, err := s.Lock(ctx)
		if err != nil {
			return err
		}
		now, err := database.WallTime(ctx, tx)
		if err != nil {
			return err
		}
		until, err := deadline(now, ttl)
		if err != nil {
			return err
		}
		if prior.Owner != owner || prior.Epoch != epoch || prior.Until.Micros <= now.Micros {
			return ErrLost
		}
		prior.Until = until
		next = prior
		return s.Put(ctx, prior, now)
	})
	if errors.Is(err, database.ErrCommitUnknown) && confirmState(ctx, b, next) == nil {
		return nil
	}
	return err
}

func confirmState(ctx context.Context, backend database.Backend, want State) error {
	check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return database.Within(check, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := FromTransaction(tx)
		if err != nil {
			return err
		}
		state, err := store.Lock(check)
		if err != nil {
			return err
		}
		if state.Owner != want.Owner || state.Epoch != want.Epoch || !want.Until.Valid || state.Until.Micros < want.Until.Micros {
			return ErrLost
		}
		return store.Assert(check, want.Owner, want.Epoch)
	})
}

func Assert(ctx context.Context, tx database.Tx, owner Owner, epoch int64) error {
	s, err := FromTransaction(tx)
	if err != nil {
		return err
	}
	return s.Assert(ctx, owner, epoch)
}
