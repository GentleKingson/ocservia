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
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	AssertLeader(ctx context.Context, tx pgx.Tx) error
	// AssertCurrent proves leadership in a dedicated transaction. It does not
	// fence any other statement.
	AssertCurrent(ctx context.Context, pool *pgxpool.Pool) error
}

// Session is one acquired leadership term. It is immutable after Acquire, so
// it can be shared across goroutines freely; the local lease deadline is
// runner state, owned and updated by the Runner that acquired the session.
type Session struct {
	identity Identity
	epoch    int64
	leaseTTL time.Duration
}

// Acquire takes the scheduler leadership lease. A takeover succeeds only
// after the previous lease expired by database time; every term, including
// a same-identity reacquire, receives a strictly higher fencing epoch.
func Acquire(ctx context.Context, pool *pgxpool.Pool, identity Identity, leaseTTL time.Duration) (*Session, error) {
	return AcquireBackend(ctx, schedulerBackend(pool), identity, leaseTTL)
}

func schedulerBackend(pool *pgxpool.Pool) database.Backend { return postgres.WrapPool(pool) }

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

// Renew extends the lease for the current term. It fails when leadership was
// taken over or the lease expired; the caller must cancel its leader context.
// Renew never mutates the immutable session; the local deadline is advanced
// by the owning Runner, anchored before this call started.
func (s *Session) Renew(ctx context.Context, pool *pgxpool.Pool) error {
	return s.RenewBackend(ctx, schedulerBackend(pool))
}

func (s *Session) RenewBackend(ctx context.Context, backend database.Backend) error {
	return schedulerlease.Renew(ctx, backend, schedulerlease.Owner{InstanceID: s.identity.InstanceID, Incarnation: s.identity.Incarnation}, s.epoch, s.leaseTTL)
}

// AssertLeader verifies inside the caller's transaction, immediately before
// commit, that this session still owns an unexpired lease with the exact
// identity and fencing epoch. The expiry check must use clock_timestamp(),
// the real wall clock: now() freezes at transaction start, so a lease that
// expired while a long fenced transaction was open would still pass an
// now()-based assert. The row share lock serializes the commit against a
// concurrent takeover update, so an assert that succeeds cannot be
// superseded before the fenced transaction commits. A rejected assert must
// abort its transaction, releasing any lock acquired before the clock recheck.
func (s *Session) AssertLeader(ctx context.Context, tx pgx.Tx) error {
	return s.AssertTransaction(ctx, postgres.WrapTx(tx))
}

func (s *Session) AssertTransaction(ctx context.Context, tx database.Tx) error {
	return schedulerlease.Assert(ctx, tx, schedulerlease.Owner{InstanceID: s.identity.InstanceID, Incarnation: s.identity.Incarnation}, s.epoch)
}

// AssertCurrent runs a dedicated transaction whose only purpose is to prove
// current leadership. It does not fence any other statement and must not be
// used to guard writes; use AssertLeader inside the writing transaction.
func (s *Session) AssertCurrent(ctx context.Context, pool *pgxpool.Pool) error {
	return s.AssertCurrentBackend(ctx, schedulerBackend(pool))
}

func (s *Session) AssertCurrentBackend(ctx context.Context, backend database.Backend) error {
	return database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error { return s.AssertTransaction(ctx, tx) })
}
