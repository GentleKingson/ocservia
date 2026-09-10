package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

const (
	localAttemptCapacity        = 16384
	localAttemptWindow          = 15 * time.Minute
	localAttemptLease           = 30 * time.Second
	localAttemptDBTimeout       = 2 * time.Second
	localAttemptLockID    int64 = 734821033
)

var (
	ErrLocalAccountLimited  = fmt.Errorf("%w: account admission refused", ErrUnauthenticated)
	ErrLocalAttemptCapacity = authstore.ErrAttemptCapacity
	errLocalAttemptCapacity = ErrLocalAttemptCapacity
)

// reserveLocalAttempt receives only normalizeLocalUsername output. A durable
// single-flight lease prevents concurrent KDF work from overshooting a threshold.
// The capacity lock and row locks are released before credential lookup or KDF.
func (s *Service) reserveLocalAttempt(ctx context.Context, username string) (uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(ctx, localAttemptDBTimeout)
	defer cancel()
	lease := uuid.New()
	var admitted bool
	err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		var err error
		admitted, err = store.ReserveAttempt(ctx, username, lease, localAttemptCapacity, localAttemptWindow, localAttemptLease)
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	if !admitted {
		return uuid.Nil, ErrLocalAccountLimited
	}
	return lease, nil
}

// Completion deliberately survives request cancellation. Only an observed bad
// password is counted; cancellation, DB/hash errors and stale credentials release
// the lease without a failure. If completion fails/crashes, the lease expires.
func (s *Service) finishLocalAttempt(username string, lease uuid.UUID, failed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), localAttemptDBTimeout)
	defer cancel()
	return s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		return store.FinishAttempt(ctx, username, lease, failed)
	})
}
