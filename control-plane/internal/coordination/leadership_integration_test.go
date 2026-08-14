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

func TestLeadershipAcquireRenewAssertIntegration(t *testing.T) {
	pool := testPool(t)
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
	ctx := context.Background()

	leader, err := Acquire(ctx, pool, mustIdentity(t), 10*time.Second)
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
	forceExpire(t, pool)

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
