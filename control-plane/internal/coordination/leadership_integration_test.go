package coordination

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

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

func mustIdentity(t *testing.T) Identity {
	t.Helper()
	identity, err := NewIdentity()
	if err != nil {
		t.Fatalf("mint identity: %v", err)
	}
	return identity
}

func forceExpire(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE scheduler_leadership SET lease_until=now()`); err != nil {
		t.Fatalf("force lease expiry: %v", err)
	}
}

// resetLeadership expires any lease left behind by an earlier test so each
// test starts from an idle, takable lease.
func resetLeadership(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE scheduler_leadership SET lease_until=now()`); err != nil {
		t.Fatalf("reset scheduler leadership: %v", err)
	}
}

func TestLeadershipAcquireRenewAssertIntegration(t *testing.T) {
	pool := testPool(t)
	resetLeadership(t, pool)
	ctx := context.Background()

	first, err := Acquire(ctx, pool, mustIdentity(t), 10*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if first.Epoch() < 1 {
		t.Fatalf("epoch must start at one or higher, got %d", first.Epoch())
	}
	if err := first.Renew(ctx, pool); err != nil {
		t.Fatalf("renew: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := first.AssertLeader(ctx, tx); err != nil {
		t.Fatalf("assert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// A second identity cannot take over while the lease is unexpired.
	if _, err := Acquire(ctx, pool, mustIdentity(t), 10*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld, got %v", err)
	}
	forceExpire(t, pool)
	second, err := Acquire(ctx, pool, mustIdentity(t), 10*time.Second)
	if err != nil {
		t.Fatalf("takeover acquire: %v", err)
	}
	if second.Epoch() <= first.Epoch() {
		t.Fatalf("takeover epoch %d must exceed old epoch %d", second.Epoch(), first.Epoch())
	}

	// The old leader cannot renew or commit after the takeover.
	if err := first.Renew(ctx, pool); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("old leader renew must fail with ErrNotLeader, got %v", err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin old leader tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := first.AssertLeader(ctx, tx); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("old leader assert must fail with ErrNotLeader, got %v", err)
	}

	// The new leader commits normally.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin new leader tx: %v", err)
	}
	defer tx2.Rollback(ctx)
	if err := second.AssertLeader(ctx, tx2); err != nil {
		t.Fatalf("new leader assert: %v", err)
	}
}

// A fencing assert holds a row share lock until commit, so a takeover that
// became eligible mid-transaction cannot bump the epoch before the fenced
// transaction commits.
func TestLeadershipAssertBlocksTakeoverIntegration(t *testing.T) {
	pool := testPool(t)
	resetLeadership(t, pool)
	ctx := context.Background()

	// A short lease lets the deadline lapse naturally while the fenced
	// transaction stays open; forcing an expiry would itself block on the
	// row lock the assert holds.
	leader, err := Acquire(ctx, pool, mustIdentity(t), 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := leader.AssertLeader(ctx, tx); err != nil {
		t.Fatalf("assert: %v", err)
	}
	time.Sleep(2 * time.Second)

	takeoverDone := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), pool, mustIdentity(t), 10*time.Second)
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

func TestRunnerRenewalLossCancelsSessionIntegration(t *testing.T) {
	pool := testPool(t)
	resetLeadership(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	runner := NewRunner(pool, mustIdentity(t), 2*time.Second, 500*time.Millisecond, nil)
	defer runner.Stop()

	var firstEpoch int64
	err := runner.WithSession(ctx, func(sessionCtx context.Context, session *Session) error {
		firstEpoch = session.Epoch()
		// Simulate a takeover: expire the lease so renewal fails.
		forceExpire(t, pool)
		_, err := Acquire(context.Background(), pool, mustIdentity(t), 10*time.Second)
		if err != nil {
			return err
		}
		select {
		case <-sessionCtx.Done():
			return nil
		case <-time.After(3 * time.Second):
			return errors.New("session context was not cancelled after leadership loss")
		}
	})
	if !errors.Is(err, ErrLeadershipLost) && err != nil {
		t.Fatalf("expected ErrLeadershipLost, got %v", err)
	}
	if err == nil {
		t.Fatal("expected ErrLeadershipLost")
	}
	if _, _, ok := runner.Session(); ok {
		t.Fatal("runner must drop the session after renewal loss")
	}

	// The next WithSession call reacquires with a strictly higher epoch.
	err = runner.WithSession(ctx, func(sessionCtx context.Context, session *Session) error {
		if session.Epoch() <= firstEpoch {
			t.Fatalf("reacquired epoch %d must exceed previous %d", session.Epoch(), firstEpoch)
		}
		return session.AssertCurrent(ctx, pool)
	})
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
}

func TestFencedExecRejectsStaleLeaderIntegration(t *testing.T) {
	pool := testPool(t)
	resetLeadership(t, pool)
	ctx := context.Background()

	leader, err := Acquire(ctx, pool, mustIdentity(t), 10*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	fenced := WithFence(ctx, leader)
	if err := FencedExec(fenced, pool, FenceFromContext(fenced), `SELECT 1`); err != nil {
		t.Fatalf("fenced exec under leadership: %v", err)
	}
	if err := FencedExec(ctx, pool, nil, `SELECT 1`); err != nil {
		t.Fatalf("legacy unfenced exec: %v", err)
	}

	forceExpire(t, pool)
	if _, err := Acquire(ctx, pool, mustIdentity(t), 10*time.Second); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	err = FencedExec(fenced, pool, FenceFromContext(fenced), `UPDATE scheduler_leadership SET updated_at=now()`)
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("stale fenced exec must fail with ErrNotLeader, got %v", err)
	}
}

// A fenced transaction that began while the lease was valid must still be
// rejected when the lease expires naturally before the pre-commit assert
// runs: the assert compares against the real wall clock, not the
// transaction clock frozen at BEGIN.
func TestLeadershipAssertRejectsMidTransactionExpiryIntegration(t *testing.T) {
	pool := testPool(t)
	resetLeadership(t, pool)
	ctx := context.Background()

	leader, err := Acquire(ctx, pool, mustIdentity(t), 1200*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	// Pin the transaction start while the lease is still valid.
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("pin transaction start: %v", err)
	}
	// Keep the transaction open until the lease lapses naturally; forcing an
	// expiry would not reproduce the frozen-transaction-clock hazard.
	time.Sleep(1600 * time.Millisecond)
	if err := leader.AssertLeader(ctx, tx); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("assert after natural mid-transaction expiry must fail with ErrNotLeader, got %v", err)
	}

	// The rejected assert must not hold the leadership row: a legitimate
	// takeover completes while the stale transaction is still open, instead
	// of queueing behind an erroneous FOR SHARE until rollback.
	takeoverDone := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), pool, mustIdentity(t), 10*time.Second)
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
}

// Session snapshots, background renewal, and leadership loss touch the same
// runner state from different goroutines. Under go test -race this test
// fails if any of those accesses is unsynchronized, and it verifies that the
// runner reacquires with strictly higher epochs through continuous
// leadership churn.
func TestRunnerConcurrentWithSessionAndLossIntegration(t *testing.T) {
	pool := testPool(t)
	resetLeadership(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	runner := NewRunner(pool, mustIdentity(t), 2*time.Second, 250*time.Millisecond, nil)
	defer runner.Stop()

	rivalIDs := []Identity{mustIdentity(t), mustIdentity(t), mustIdentity(t)}
	rivalDone := make(chan struct{})
	go func() {
		defer close(rivalDone)
		for _, rival := range rivalIDs {
			if _, err := pool.Exec(context.Background(), `UPDATE scheduler_leadership SET lease_until=now()`); err != nil {
				return
			}
			// Losing the race against the runner under test is fine; the
			// point is to keep leadership changing underneath it.
			if _, err := Acquire(context.Background(), pool, rival, 600*time.Millisecond); err == nil {
				time.Sleep(700 * time.Millisecond)
			}
		}
	}()

	var lastEpoch int64
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		err := runner.WithSession(ctx, func(sessionCtx context.Context, session *Session) error {
			if session.Epoch() < lastEpoch {
				return errors.New("runner exposed an epoch lower than a previous session")
			}
			lastEpoch = session.Epoch()
			select {
			case <-sessionCtx.Done():
				return nil
			case <-time.After(40 * time.Millisecond):
				return nil
			}
		})
		if err != nil && !errors.Is(err, ErrLeadershipLost) && !errors.Is(err, ErrNotLeader) {
			t.Fatalf("with session: %v", err)
		}
	}
	<-rivalDone

	err := runner.WithSession(ctx, func(sessionCtx context.Context, session *Session) error {
		return session.AssertCurrent(sessionCtx, pool)
	})
	if err != nil {
		t.Fatalf("reacquire after churn: %v", err)
	}
}
