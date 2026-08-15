// Package connectionowner implements the per-node connection-owner lease
// defined by the G6 HA fencing contract: at most one unexpired owner per
// node, a monotonically increasing fencing epoch that is never reused, and
// transaction-time asserts so a stale owner can never commit after a
// takeover.
package connectionowner

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// Identity binds an ownership term to exactly one controller process
// incarnation, mirroring the scheduler leadership identity.
type Identity struct {
	InstanceID  uuid.UUID
	Incarnation int64
}

// Term is one acquired node ownership lease. It is immutable after Acquire;
// the local lease deadline is runner state owned by the caller.
type Term struct {
	identity     Identity
	nodeID       [16]byte
	connectionID [16]byte
	epoch        int64
	leaseTTL     time.Duration
}

// Acquire takes the ownership lease for one node. A first term starts at
// epoch 1; every later term, including a same-identity reconnect with a new
// connection, receives a strictly higher epoch. A different process
// incarnation may only take over after the previous lease expired by
// PostgreSQL time; the same process incarnation may replace its own
// connection immediately.
func Acquire(ctx context.Context, pool *pgxpool.Pool, nodeID [16]byte, identity Identity, connectionID [16]byte, leaseTTL time.Duration) (*Term, error) {
	if leaseTTL <= 0 {
		return nil, errors.New("connectionowner: lease TTL must be positive")
	}
	var epoch int64
	err := pool.QueryRow(ctx, `INSERT INTO connection_owner_fencing
		(node_id, owner_instance_id, owner_incarnation, connection_id, owner_epoch, lease_until, updated_at)
		VALUES ($1, $2, $3, $4, 1, now()+$5::interval, now())
		ON CONFLICT (node_id) DO UPDATE SET
			owner_instance_id=EXCLUDED.owner_instance_id,
			owner_incarnation=EXCLUDED.owner_incarnation,
			connection_id=EXCLUDED.connection_id,
			owner_epoch=connection_owner_fencing.owner_epoch+1,
			lease_until=EXCLUDED.lease_until,
			updated_at=now()
		WHERE connection_owner_fencing.lease_until<=now()
			OR (connection_owner_fencing.owner_instance_id=EXCLUDED.owner_instance_id
				AND connection_owner_fencing.owner_incarnation=EXCLUDED.owner_incarnation)
		RETURNING owner_epoch`,
		nodeID[:], identity.InstanceID, identity.Incarnation, connectionID[:], leaseTTL.String()).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLeaseHeld
	}
	if err != nil {
		return nil, fmt.Errorf("connectionowner: acquire node ownership: %w", err)
	}
	return &Term{identity: identity, nodeID: nodeID, connectionID: connectionID, epoch: epoch, leaseTTL: leaseTTL}, nil
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

// Renew extends the lease for the current term only. It fails when ownership
// was taken over or the lease expired; the caller must cancel its
// owner-scoped work. Renew never mutates the immutable term.
func (t *Term) Renew(ctx context.Context, pool *pgxpool.Pool) error {
	tag, err := pool.Exec(ctx, `UPDATE connection_owner_fencing
		SET lease_until=now()+$6::interval, updated_at=now()
		WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>now()`,
		t.nodeID[:], t.identity.InstanceID, t.identity.Incarnation, t.connectionID[:], t.epoch, t.leaseTTL.String())
	if err != nil {
		return fmt.Errorf("connectionowner: renew node ownership: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotOwner
	}
	return nil
}

// AssertFenced verifies inside the caller's transaction, immediately before
// commit, that this term still owns the node with the exact identity,
// connection, and fencing epoch. The expiry check uses clock_timestamp(),
// the real wall clock, because now() freezes at transaction start; the row
// share lock serializes the commit against a concurrent takeover, while a
// rejected assert takes no row lock and never blocks a takeover.
func (t *Term) AssertFenced(ctx context.Context, tx pgx.Tx) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM connection_owner_fencing
		WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>clock_timestamp()
		FOR SHARE OF connection_owner_fencing`,
		t.nodeID[:], t.identity.InstanceID, t.identity.Incarnation, t.connectionID[:], t.epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotOwner
	}
	if err != nil {
		return fmt.Errorf("connectionowner: assert node ownership: %w", err)
	}
	return nil
}

// AssertCurrent runs a dedicated transaction whose only purpose is to prove
// current ownership. It does not fence any other statement and must not be
// used to guard writes; use AssertFenced inside the writing transaction.
func (t *Term) AssertCurrent(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := t.AssertFenced(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// OwnerState is a point-in-time read of one node's ownership row.
type OwnerState struct {
	InstanceID      uuid.UUID
	Incarnation     int64
	ConnectionID    [16]byte
	Epoch           int64
	LeaseUntilValid bool
}

// ReadState returns the current ownership row for one node, if any.
func ReadState(ctx context.Context, pool *pgxpool.Pool, nodeID [16]byte) (OwnerState, error) {
	var state OwnerState
	var connectionID []byte
	err := pool.QueryRow(ctx, `SELECT owner_instance_id, owner_incarnation, connection_id, owner_epoch, lease_until>clock_timestamp()
		FROM connection_owner_fencing WHERE node_id=$1`, nodeID[:]).
		Scan(&state.InstanceID, &state.Incarnation, &connectionID, &state.Epoch, &state.LeaseUntilValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnerState{}, fmt.Errorf("connectionowner: node %x has no ownership row", nodeID)
	}
	if err != nil {
		return OwnerState{}, fmt.Errorf("connectionowner: read node ownership: %w", err)
	}
	copy(state.ConnectionID[:], connectionID)
	return state, nil
}
