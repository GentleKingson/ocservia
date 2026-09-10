// Package connectionowner implements the per-node connection-owner lease
// defined by the G6 HA fencing contract: at most one unexpired owner per
// node, a monotonically increasing fencing epoch that is never reused, and
// transaction-time asserts so a stale owner can never commit after a
// takeover.
package connectionowner

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotOwner is returned when the connection no longer owns the node lease,
// either because another owner took over or because the lease expired.
// Callers must stop dispatching, renewing, closing, and committing
// connection-scoped state.
var ErrNotOwner = errors.New("connectionowner: connection ownership lost")

// ErrLeaseHeld reports that another unexpired owner currently holds the
// node lease. Cross-instance takeover requires the previous lease to expire.
var ErrLeaseHeld = errors.New("connectionowner: node lease held by another owner")

// ErrNoOwnerRow reports that the node has no ownership row at all: it was
// never fenced. A fence registered elsewhere without a backing row claims a
// term the authority never granted or no longer knows, so readers must fail
// closed on it.
var ErrNoOwnerRow = errors.New("connectionowner: node has no ownership row")

// Identity binds an ownership term to exactly one controller process
// incarnation, mirroring the scheduler leadership identity.
type Identity = ownerstore.Identity

// Term is one acquired node ownership lease. It is immutable after Acquire.
// LeaseUntil is the exact deadline the database recorded for the term; callers
// must use it instead of reconstructing a local deadline from the TTL, and
// must replace it with the exact deadline returned by Renew.
type Term struct {
	identity     Identity
	nodeID       [16]byte
	connectionID [16]byte
	epoch        int64
	leaseTTL     time.Duration
	leaseUntil   time.Time
}

// Acquire takes the ownership lease for one node. A first term starts at
// epoch 1; every later term, including a same-identity reconnect with a new
// connection, receives a strictly higher epoch. A different process
// incarnation may only take over after the previous lease expired by
// PostgreSQL time; the same process incarnation may replace its own
// connection immediately.
func Acquire(ctx context.Context, pool *pgxpool.Pool, nodeID [16]byte, identity Identity, connectionID [16]byte, leaseTTL time.Duration) (*Term, error) {
	return AcquireBackend(ctx, postgres.WrapPool(pool), nodeID, identity, connectionID, leaseTTL)
}

// Epoch returns the fencing epoch of this term. Per-node epochs increase
// monotonically across terms and are never reused, including across schema
// rollback and re-upgrade cycles.
func (t *Term) Epoch() int64 { return t.epoch }

// NodeID returns the node this term owns.
func (t *Term) NodeID() [16]byte { return t.nodeID }

// ConnectionID returns the connection identity bound to this term.
func (t *Term) ConnectionID() [16]byte { return t.connectionID }

// Identity returns the owner identity bound to this term.
func (t *Term) Identity() Identity { return t.identity }

// LeaseUntil returns the exact lease deadline the database recorded when this
// term was acquired. The deadline is authoritative: reconstructing it locally
// with the TTL and a local clock is forbidden because clock drift would let a
// stale owner treat an expired lease as valid.
func (t *Term) LeaseUntil() time.Time { return t.leaseUntil }

// LeaseTTL returns the configured TTL of this term. It is configuration, not
// an authority: deadlines come from LeaseUntil and Renew.
func (t *Term) LeaseTTL() time.Duration { return t.leaseTTL }

// Renew extends the lease for the current term only. It fails when ownership
// was taken over or the lease expired; the caller must cancel its
// owner-scoped work. Renew returns the exact new lease deadline PostgreSQL
// recorded; the caller must replace any stored deadline with it.
func (t *Term) Renew(ctx context.Context, pool *pgxpool.Pool) (time.Time, error) {
	return t.RenewBackend(ctx, postgres.WrapPool(pool))
}

// Release expires this exact term's lease at PostgreSQL time so a successor
// can take over without waiting out the TTL. The exact-term predicate keeps a
// late release from touching a successor's row: when the term was already
// taken over, Release reports ErrNotOwner and changes nothing. The epoch is
// never decremented or reset; only the deadline of the matching row moves to
// now, preserving monotonic takeover.
func (t *Term) Release(ctx context.Context, pool *pgxpool.Pool) error {
	return t.ReleaseBackend(ctx, postgres.WrapPool(pool))
}

// AssertFenced verifies inside the caller's transaction, immediately before
// commit, that this term still owns the node with the exact identity,
// connection, and fencing epoch. The expiry check uses clock_timestamp(),
// the real wall clock, because now() freezes at transaction start; the row
// share lock serializes the commit against a concurrent takeover, while a
// rejected assert takes no row lock and never blocks a takeover.
func (t *Term) AssertFenced(ctx context.Context, tx pgx.Tx) error {
	return t.AssertTransaction(ctx, postgres.WrapTx(tx))
}

// AssertCurrent runs a dedicated transaction whose only purpose is to prove
// current ownership. It does not fence any other statement and must not be
// used to guard writes; use AssertFenced inside the writing transaction.
func (t *Term) AssertCurrent(ctx context.Context, pool *pgxpool.Pool) error {
	return t.AssertCurrentBackend(ctx, postgres.WrapPool(pool))
}

// GuardObservedTerm proves one observed term is still the node's current
// ownership row and opens a fencing interval around it: the guard's
// transaction holds a FOR SHARE lock on the row, so any Acquire that would
// advance the epoch waits until the returned release function runs. Callers
// that verify a term they do not own — a Controller role reading a fence
// registered in transportd — must run the mutation carrying that term's
// proofs before release, which makes PostgreSQL the mutation-time ownership
// authority instead of a point-in-time check. The assert requires an
// unexpired lease at clock_timestamp(); keeping the interval inside the
// caller's bounded RPC deadline is the caller's responsibility.
func GuardObservedTerm(ctx context.Context, pool *pgxpool.Pool, nodeID [16]byte, instanceID uuid.UUID, incarnation int64, connectionID [16]byte, epoch int64) (release func() error, err error) {
	return GuardObservedTermBackend(ctx, postgres.WrapPool(pool), nodeID, instanceID, incarnation, connectionID, epoch)
}

// OwnerState is a point-in-time read of one node's ownership row.
type OwnerState = ownerstore.State

// ReadState returns the current ownership row for one node, if any.
func ReadState(ctx context.Context, pool *pgxpool.Pool, nodeID [16]byte) (OwnerState, error) {
	return ReadStateBackend(ctx, postgres.WrapPool(pool), nodeID)
}
