package connectionowner

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testIdentity(t *testing.T) Identity {
	t.Helper()
	return Identity{InstanceID: uuid.New(), Incarnation: time.Now().UnixNano()}
}

func testNode(t *testing.T) [16]byte {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint node id: %v", err)
	}
	var node [16]byte
	copy(node[:], id[:])
	return node
}

func testConnection(t *testing.T) [16]byte {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint connection id: %v", err)
	}
	var connection [16]byte
	copy(connection[:], id[:])
	return connection
}

func forceExpire(t *testing.T, pool *pgxpool.Pool, node [16]byte) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE connection_owner_fencing SET lease_until=clock_timestamp() WHERE node_id=$1`, node[:]); err != nil {
		t.Fatalf("force lease expiry: %v", err)
	}
}

// assertExactLeaseDeadline proves the deadline handed to the caller is the
// exact value PostgreSQL stored, not a local reconstruction.
func assertExactLeaseDeadline(t *testing.T, pool *pgxpool.Pool, node [16]byte, deadline time.Time) {
	t.Helper()
	var stored time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT lease_until FROM connection_owner_fencing WHERE node_id=$1`, node[:]).Scan(&stored); err != nil {
		t.Fatalf("read stored lease deadline: %v", err)
	}
	stored = stored.UTC()
	if !deadline.Equal(stored) {
		t.Fatalf("handed deadline %v != stored deadline %v", deadline, stored)
	}
}

// TestConnectionOwnerExactLeaseDeadlineHandoffIntegration locks the Stage 4
// precondition that Acquire and Renew hand back the exact PostgreSQL deadline
// with microsecond fidelity: any drift would let an owner sign a fence whose
// lease outlives the authority's.
func TestConnectionOwnerExactLeaseDeadlineHandoffIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	node := testNode(t)

	term, err := Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 90*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	assertExactLeaseDeadline(t, pool, node, term.LeaseUntil())
	if remainder := time.Until(term.LeaseUntil()); remainder <= 0 || remainder > 90*time.Second {
		t.Fatalf("acquire deadline remainder %v outside (0, ttl]", remainder)
	}

	renewedUntil, err := term.Renew(ctx, pool)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	assertExactLeaseDeadline(t, pool, node, renewedUntil)
	if !renewedUntil.After(term.LeaseUntil()) {
		t.Fatalf("renew deadline %v must strictly extend %v", renewedUntil, term.LeaseUntil())
	}
}

func TestConnectionOwnerAcquireRenewAssertIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	nodeA, nodeB := testNode(t), testNode(t)
	identity := testIdentity(t)

	term, err := Acquire(ctx, pool, nodeA, identity, testConnection(t), 2*time.Second)
	if err != nil || term.Epoch() != 1 {
		t.Fatalf("first acquire = (%v, %v), want epoch 1", term, err)
	}
	assertExactLeaseDeadline(t, pool, nodeA, term.LeaseUntil())
	renewedUntil, err := term.Renew(ctx, pool)
	if err != nil {
		t.Fatalf("renew current term: %v", err)
	}
	assertExactLeaseDeadline(t, pool, nodeA, renewedUntil)
	if !renewedUntil.After(term.LeaseUntil()) {
		t.Fatalf("renew deadline %v must extend acquire deadline %v", renewedUntil, term.LeaseUntil())
	}
	if err := term.AssertCurrent(ctx, pool); err != nil {
		t.Fatalf("assert current term: %v", err)
	}

	// Nodes fence independently: a second node starts at its own epoch one.
	other, err := Acquire(ctx, pool, nodeB, testIdentity(t), testConnection(t), 2*time.Second)
	if err != nil || other.Epoch() != 1 {
		t.Fatalf("independent node acquire = (%v, %v), want epoch 1", other, err)
	}

	state, err := ReadState(ctx, pool, nodeA)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.InstanceID != identity.InstanceID || state.Epoch != 1 || !state.LeaseUntilValid {
		t.Fatalf("unexpected owner state: %+v", state)
	}
}

func TestConnectionOwnerCrossInstanceTakeoverRequiresExpiryIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	node := testNode(t)

	first, err := Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 30*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// An unexpired lease blocks a different process incarnation.
	if _, err := Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 30*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("takeover before expiry = %v, want ErrLeaseHeld", err)
	}

	forceExpire(t, pool, node)
	second, err := Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 30*time.Second)
	if err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}
	if second.Epoch() <= first.Epoch() {
		t.Fatalf("takeover epoch = %d, want > %d", second.Epoch(), first.Epoch())
	}

	// The fenced-out owner must fail renew and assert.
	if _, err := first.Renew(ctx, pool); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale renew = %v, want ErrNotOwner", err)
	}
	if err := first.AssertCurrent(ctx, pool); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale assert = %v, want ErrNotOwner", err)
	}
	if err := second.AssertCurrent(ctx, pool); err != nil {
		t.Fatalf("new owner assert: %v", err)
	}
}

func TestConnectionOwnerSameOwnerNewConnectionIncrementsEpochIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	node := testNode(t)
	identity := testIdentity(t)

	first, err := Acquire(ctx, pool, node, identity, testConnection(t), 30*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// The same process incarnation may replace its own connection without
	// waiting for lease expiry, but never reuses an epoch.
	second, err := Acquire(ctx, pool, node, identity, testConnection(t), 30*time.Second)
	if err != nil {
		t.Fatalf("same-owner connection replacement: %v", err)
	}
	if second.Epoch() <= first.Epoch() {
		t.Fatalf("replacement epoch = %d, want > %d", second.Epoch(), first.Epoch())
	}
	// The replaced connection is fenced out immediately.
	if _, err := first.Renew(ctx, pool); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("replaced connection renew = %v, want ErrNotOwner", err)
	}
	if err := first.AssertCurrent(ctx, pool); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("replaced connection assert = %v, want ErrNotOwner", err)
	}

	// A restarted incarnation of the same instance is a different owner and
	// must wait for lease expiry like any cross-instance takeover.
	restarted := Identity{InstanceID: identity.InstanceID, Incarnation: identity.Incarnation + 1}
	if _, err := Acquire(ctx, pool, node, restarted, testConnection(t), 30*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("restarted incarnation takeover before expiry = %v, want ErrLeaseHeld", err)
	}
}

func TestConnectionOwnerAssertRejectsMidTransactionExpiryIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	node := testNode(t)

	term, err := Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 1200*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Pin the transaction start while the lease is still valid, without
	// taking any row lock the rejected assert could be blamed for.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("pin transaction start: %v", err)
	}
	// Keep the transaction open until the lease lapses naturally; forcing an
	// expiry would not reproduce the frozen-transaction-clock hazard.
	time.Sleep(1600 * time.Millisecond)

	// clock_timestamp() must reject the assert even though now() froze at a
	// time when the lease was still valid.
	if err := term.AssertFenced(ctx, tx); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("mid-transaction assert = %v, want ErrNotOwner", err)
	}

	// The rejected assert must not hold the ownership row: a legitimate
	// takeover completes while the stale transaction is still open, instead
	// of queueing behind an erroneous FOR SHARE until rollback.
	takeoverDone := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), pool, node, testIdentity(t), testConnection(t), 10*time.Second)
		takeoverDone <- err
	}()
	select {
	case err := <-takeoverDone:
		if err != nil {
			t.Fatalf("takeover after rejected assert: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rejected assert must not block takeover until the stale transaction rolls back")
	}
	if _, err := term.Renew(ctx, pool); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale renew after takeover = %v, want ErrNotOwner", err)
	}
}

// A fencing assert holds a row share lock until commit, so a takeover that
// became eligible mid-transaction cannot bump the epoch before the fenced
// transaction commits.
func TestConnectionOwnerAssertBlocksTakeoverIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	node := testNode(t)

	// A short lease lets the deadline lapse naturally while the fenced
	// transaction stays open; forcing an expiry would itself block on the
	// row lock the assert holds.
	term, err := Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := term.AssertFenced(ctx, tx); err != nil {
		t.Fatalf("assert: %v", err)
	}
	time.Sleep(2 * time.Second)

	takeoverDone := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), pool, node, testIdentity(t), testConnection(t), 10*time.Second)
		takeoverDone <- err
	}()
	select {
	case err := <-takeoverDone:
		t.Fatalf("takeover must block while the fenced transaction is open, got %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fenced tx: %v", err)
	}
	select {
	case err := <-takeoverDone:
		if err != nil {
			t.Fatalf("takeover after commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeover did not finish after the fenced transaction committed")
	}
}

// TestConnectionOwnerTakeoverContinuesPastRetainedEpochIntegration proves the
// real Acquire path continues past a retained per-node epoch on a re-upgraded
// schema. It is environment-gated so the migration lifecycle harness can
// point it at the exact node whose epoch survived a rollback cycle.
func TestConnectionOwnerTakeoverContinuesPastRetainedEpochIntegration(t *testing.T) {
	nodeHex := os.Getenv("OCSERV_TEST_RETAINED_NODE_HEX")
	if nodeHex == "" {
		t.Skip("OCSERV_TEST_RETAINED_NODE_HEX not set")
	}
	raw, err := hex.DecodeString(nodeHex)
	if err != nil || len(raw) != 16 {
		t.Fatalf("invalid OCSERV_TEST_RETAINED_NODE_HEX: %q", nodeHex)
	}
	pool := testPool(t)
	ctx := context.Background()
	var node [16]byte
	copy(node[:], raw)

	retained, err := ReadState(ctx, pool, node)
	if err != nil {
		t.Fatalf("read retained ownership state: %v", err)
	}
	if retained.Epoch < 1 {
		t.Fatalf("retained epoch must be at least one, got %d", retained.Epoch)
	}
	forceExpire(t, pool, node)
	term, err := Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 30*time.Second)
	if err != nil {
		t.Fatalf("real takeover over retained state: %v", err)
	}
	if term.Epoch() <= retained.Epoch {
		t.Fatalf("takeover epoch = %d, want > retained epoch %d", term.Epoch(), retained.Epoch)
	}
	if err := term.AssertCurrent(ctx, pool); err != nil {
		t.Fatalf("new owner assert after takeover: %v", err)
	}
}

func TestConnectionOwnerEpochNeverReusedIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	node := testNode(t)
	seen := map[int64]bool{}
	previous := int64(0)
	identity := testIdentity(t)

	// A chain of takeovers, including a same-identity reacquire, must hand
	// out strictly increasing epochs that were never observed before.
	for index := 0; index < 6; index++ {
		forceExpire(t, pool, node)
		var term *Term
		var err error
		if index == 3 {
			term, err = Acquire(ctx, pool, node, identity, testConnection(t), 5*time.Second)
		} else {
			term, err = Acquire(ctx, pool, node, testIdentity(t), testConnection(t), 5*time.Second)
		}
		if err != nil {
			t.Fatalf("takeover %d: %v", index, err)
		}
		if term.Epoch() <= previous || seen[term.Epoch()] {
			t.Fatalf("takeover %d epoch = %d, previous = %d, reused = %v", index, term.Epoch(), previous, seen[term.Epoch()])
		}
		seen[term.Epoch()] = true
		previous = term.Epoch()
	}
}

// TestConnectionOwnerGuardObservedTermIntegration pins the observer-side
// fencing interval: the guard accepts exactly the current ownership row —
// matching instance, incarnation, connection, and epoch under an unexpired
// lease — and rejects every other term with ErrNotOwner. While the guard is
// held, a same-identity Acquire that would advance the epoch blocks until
// the guard is released, which is what makes the authority span the caller's
// mutation instead of being a point-in-time check.
func TestConnectionOwnerGuardObservedTermIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	node := testNode(t)
	identity := testIdentity(t)
	term, err := Acquire(ctx, pool, node, identity, testConnection(t), 30*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	release, err := GuardObservedTerm(ctx, pool, node, identity.InstanceID, identity.Incarnation, term.ConnectionID(), term.Epoch())
	if err != nil {
		t.Fatalf("guard the exact term: %v", err)
	}

	// While the guard holds the row, an Acquire that would advance the epoch
	// — including a same-identity reconnect, which needs no expiry — must
	// wait for the fencing interval to end.
	acquired := make(chan error, 1)
	go func() {
		_, err := Acquire(ctx, pool, node, identity, testConnection(t), 30*time.Second)
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("epoch advanced while the guard was held: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if err := release(); err != nil {
		t.Fatalf("release the guard: %v", err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("acquire after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire did not proceed after the guard was released")
	}

	// The successor's term is now the current row; the old term's guard must
	// fail closed, as must every other mismatch of the exact-term predicate.
	successor, err := ReadState(ctx, pool, node)
	if err != nil {
		t.Fatalf("read successor state: %v", err)
	}
	if _, err := GuardObservedTerm(ctx, pool, node, identity.InstanceID, identity.Incarnation, term.ConnectionID(), term.Epoch()); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale term guard = %v, want ErrNotOwner", err)
	}
	otherInstance := identity.InstanceID
	otherInstance[0] ^= 1
	otherConnection := term.ConnectionID()
	otherConnection[0] ^= 1
	mismatches := []struct {
		name         string
		instanceID   uuid.UUID
		incarnation  int64
		connectionID [16]byte
		epoch        int64
	}{
		{"successor epoch with a live lease", successor.InstanceID, successor.Incarnation, successor.ConnectionID, successor.Epoch - 1},
		{"different owner instance", otherInstance, successor.Incarnation, successor.ConnectionID, successor.Epoch},
		{"different incarnation", successor.InstanceID, successor.Incarnation + 1, successor.ConnectionID, successor.Epoch},
		{"different connection", successor.InstanceID, successor.Incarnation, otherConnection, successor.Epoch},
	}
	for _, mismatch := range mismatches {
		if _, err := GuardObservedTerm(ctx, pool, node, mismatch.instanceID, mismatch.incarnation, mismatch.connectionID, mismatch.epoch); !errors.Is(err, ErrNotOwner) {
			t.Fatalf("%s guard = %v, want ErrNotOwner", mismatch.name, err)
		}
	}

	// The exact current term guards again, but an expired lease does not.
	forceExpire(t, pool, node)
	if _, err := GuardObservedTerm(ctx, pool, node, successor.InstanceID, successor.Incarnation, successor.ConnectionID, successor.Epoch); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("expired lease guard = %v, want ErrNotOwner", err)
	}

	// A node with no ownership row at all is not guardable either.
	if _, err := GuardObservedTerm(ctx, pool, testNode(t), successor.InstanceID, successor.Incarnation, successor.ConnectionID, successor.Epoch); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("missing row guard = %v, want ErrNotOwner", err)
	}
}
