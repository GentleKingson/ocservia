// Package coordination implements the fenced scheduler leadership lease
// defined by the G6 HA contract: one leadership term per maintenance session,
// renewed while the session runs, cancelled when renewal fails, and enforced
// transaction-by-transaction so an expired leader can never commit.
package coordination

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
	"github.com/google/uuid"
)

// ErrNotLeader is returned when the session no longer holds the fencing
// epoch, either because another instance took over or because the lease
// expired. Callers must abort the transaction and stop scheduling.
var ErrNotLeader = schedulerlease.ErrLost

// ErrLeaseHeld reports that another unexpired leader currently owns the lease.
var ErrLeaseHeld = schedulerlease.ErrHeld

// Identity binds a leadership term to exactly one process incarnation. The
// incarnation is derived from process start, so a restarted process on the
// same instance identity can never reuse a previous term.
type Identity struct {
	InstanceID  uuid.UUID
	Incarnation int64
}

// NewIdentity mints a per-process identity. The instance identifier is random
// per call, so callers must create it once per process and share it.
func NewIdentity() (Identity, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Identity{}, fmt.Errorf("coordination: mint instance identity: %w", err)
	}
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return Identity{}, fmt.Errorf("coordination: mint incarnation: %w", err)
	}
	incarnation := time.Now().UnixNano()
	for _, b := range seed {
		incarnation = incarnation*31 + int64(b)
	}
	if incarnation < 0 {
		incarnation = -incarnation
	}
	return Identity{InstanceID: id, Incarnation: incarnation}, nil
}

// Fence asserts leadership inside a caller-owned transaction before commit.
type Fence interface {
	AssertTransaction(ctx context.Context, tx database.Tx) error
}

// Session is one acquired leadership term. It is immutable after Acquire, so
// it can be shared across goroutines freely; the local lease deadline is
// runner state, owned and updated by the Runner that acquired the session.
type Session struct {
	identity Identity
	epoch    int64
	leaseTTL time.Duration
}

func AcquireBackend(ctx context.Context, backend database.Backend, identity Identity, leaseTTL time.Duration) (*Session, error) {
	epoch, err := schedulerlease.Acquire(ctx, backend, schedulerlease.Owner{InstanceID: identity.InstanceID, Incarnation: identity.Incarnation}, leaseTTL)
	if err != nil {
		return nil, fmt.Errorf("coordination: acquire scheduler leadership: %w", err)
	}
	return &Session{identity: identity, epoch: epoch, leaseTTL: leaseTTL}, nil
}

// Epoch returns the fencing epoch of this term. Epochs increase monotonically
// across terms and are never reused.
func (s *Session) Epoch() int64 { return s.epoch }

// Identity returns the owner identity bound to this term.
func (s *Session) Identity() Identity { return s.identity }

func (s *Session) RenewBackend(ctx context.Context, backend database.Backend) error {
	return schedulerlease.Renew(ctx, backend, schedulerlease.Owner{InstanceID: s.identity.InstanceID, Incarnation: s.identity.Incarnation}, s.epoch, s.leaseTTL)
}

func (s *Session) AssertTransaction(ctx context.Context, tx database.Tx) error {
	return schedulerlease.Assert(ctx, tx, schedulerlease.Owner{InstanceID: s.identity.InstanceID, Incarnation: s.identity.Incarnation}, s.epoch)
}

func (s *Session) AssertCurrentBackend(ctx context.Context, backend database.Backend) error {
	return database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error { return s.AssertTransaction(ctx, tx) })
}
