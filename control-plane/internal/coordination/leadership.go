// Package coordination implements the fenced scheduler leadership lease
// defined by the G6 HA contract: one leadership term per maintenance session,
// renewed while the session runs, cancelled when renewal fails, and enforced
// transaction-by-transaction so an expired leader can never commit.
package coordination

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotLeader is returned when the session no longer holds the fencing
// epoch, either because another instance took over or because the lease
// expired. Callers must abort the transaction and stop scheduling.
var ErrNotLeader = errors.New("coordination: scheduler leadership lost")

// ErrLeaseHeld reports that another unexpired leader currently owns the lease.
var ErrLeaseHeld = errors.New("coordination: scheduler lease held by another leader")

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

// Session is one acquired leadership term. It is safe for concurrent use.
type Session struct {
	identity Identity
	epoch    int64
	leaseTTL time.Duration
	// localExpiry is a monotonic local deadline derived from the last
	// successful acquire or renew. Once passed, the process must stop
	// starting new fenced work even before PostgreSQL confirms the loss.
	localExpiry time.Time
}

// Acquire takes the scheduler leadership lease. A takeover succeeds only
// after the previous lease expired by PostgreSQL time; every term, including
// a same-identity reacquire, receives a strictly higher fencing epoch.
func Acquire(ctx context.Context, pool *pgxpool.Pool, identity Identity, leaseTTL time.Duration) (*Session, error) {
	if leaseTTL <= 0 {
		return nil, errors.New("coordination: lease TTL must be positive")
	}
	var epoch int64
	err := pool.QueryRow(ctx, `UPDATE scheduler_leadership
		SET instance_id=$1, incarnation=$2, epoch=epoch+1, lease_until=now()+$3::interval, updated_at=now()
		WHERE id=1 AND (lease_until<=now() OR (instance_id=$1 AND incarnation=$2))
		RETURNING epoch`, identity.InstanceID, identity.Incarnation, leaseTTL.String()).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLeaseHeld
	}
	if err != nil {
		return nil, fmt.Errorf("coordination: acquire scheduler leadership: %w", err)
	}
	return &Session{identity: identity, epoch: epoch, leaseTTL: leaseTTL, localExpiry: time.Now().Add(leaseTTL)}, nil
}

// Epoch returns the fencing epoch of this term. Epochs increase monotonically
// across terms and are never reused.
func (s *Session) Epoch() int64 { return s.epoch }

// Identity returns the owner identity bound to this term.
func (s *Session) Identity() Identity { return s.identity }

// ExpiredLocally reports whether the local monotonic deadline has passed and
// the process must stop starting new fenced work.
func (s *Session) ExpiredLocally(now time.Time) bool {
	return !now.Before(s.localExpiry)
}

// Renew extends the lease for the current term. It fails when leadership was
// taken over or the lease expired; the caller must cancel its leader context.
func (s *Session) Renew(ctx context.Context, pool *pgxpool.Pool) error {
	tag, err := pool.Exec(ctx, `UPDATE scheduler_leadership
		SET lease_until=now()+$4::interval, updated_at=now()
		WHERE id=1 AND instance_id=$1 AND incarnation=$2 AND epoch=$3 AND lease_until>now()`,
		s.identity.InstanceID, s.identity.Incarnation, s.epoch, s.leaseTTL.String())
	if err != nil {
		return fmt.Errorf("coordination: renew scheduler leadership: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotLeader
	}
	s.localExpiry = time.Now().Add(s.leaseTTL)
	return nil
}

// AssertLeader verifies inside the caller's transaction, immediately before
// commit, that this session still owns an unexpired lease with the exact
// identity and fencing epoch. The row share lock serializes the commit
// against a concurrent takeover update, so an assert that succeeds cannot be
// superseded before the fenced transaction commits.
func (s *Session) AssertLeader(ctx context.Context, tx pgx.Tx) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM scheduler_leadership
		WHERE id=1 AND instance_id=$1 AND incarnation=$2 AND epoch=$3 AND lease_until>now()
		FOR SHARE OF scheduler_leadership`,
		s.identity.InstanceID, s.identity.Incarnation, s.epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotLeader
	}
	if err != nil {
		return fmt.Errorf("coordination: assert scheduler leadership: %w", err)
	}
	return nil
}

// AssertCurrent runs a dedicated transaction whose only purpose is to prove
// current leadership. It does not fence any other statement and must not be
// used to guard writes; use AssertLeader inside the writing transaction.
func (s *Session) AssertCurrent(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := s.AssertLeader(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
