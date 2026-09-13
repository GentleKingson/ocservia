// Package connectionowner implements the per-node connection-owner lease
// defined by the G6 HA fencing contract: at most one unexpired owner per
// node, a monotonically increasing fencing epoch that is never reused, and
// transaction-time asserts so a stale owner can never commit after a
// takeover.
package connectionowner

import (
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
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

// OwnerState is a point-in-time read of one node's ownership row.
type OwnerState = ownerstore.State
