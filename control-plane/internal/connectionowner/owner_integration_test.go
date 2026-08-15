package connectionowner

import (
	"context"
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

func TestConnectionOwnerAcquireRenewAssertIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	nodeA, nodeB := testNode(t), testNode(t)
	identity := testIdentity(t)

	term, err := Acquire(ctx, pool, nodeA, identity, testConnection(t), 2*time.Second)
	if err != nil || term.Epoch() != 1 {
		t.Fatalf("first acquire = (%v, %v), want epoch 1", term, err)
	}
	if err := term.Renew(ctx, pool); err != nil {
		t.Fatalf("renew current term: %v", err)
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
	if err := first.Renew(ctx, pool); !errors.Is(err, ErrNotOwner) {
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
	if err := first.Renew(ctx, pool); !errors.Is(err, ErrNotOwner) {
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

	// Pin the ownership row with a stale fencing transaction whose snapshot
	// saw a valid lease, then hold it open past natural expiry.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM connection_owner_fencing WHERE node_id=$1 FOR SHARE`, node[:]); err != nil {
		t.Fatalf("pin row: %v", err)
	}
	time.Sleep(1600 * time.Millisecond)

	// clock_timestamp() must reject the assert even though now() froze at a
	// time when the lease was still valid.
	if err := term.AssertFenced(ctx, tx); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("mid-transaction assert = %v, want ErrNotOwner", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback stale transaction: %v", err)
	}

	// The rejected assert must not block a legitimate takeover.
	done := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), pool, node, testIdentity(t), testConnection(t), 5*time.Second)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("takeover after rejected assert: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeover blocked after rejected assert")
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
