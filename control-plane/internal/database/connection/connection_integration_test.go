//go:build integration

package connection

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestControllerLockSessionLoss(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	conn, err := Open(ctx, Options{Environment: "test", URL: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	monitor, release, err := conn.AcquireControllerLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, duplicateRelease, err := conn.AcquireControllerLock(ctx); err == nil {
		duplicateRelease()
		t.Fatal("second Controller acquired the same lock")
	}
	result := make(chan error, 1)
	go func() {
		defer close(result)
		result <- monitor(ctx)
	}()
	defer func() {
		cancel()
		<-result
	}()
	const lockSession = `SELECT pid FROM pg_locks
		WHERE locktype='advisory' AND granted AND objsubid=1
		AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
		AND classid=($1::bigint >> 32)::oid AND objid=($1::bigint & 4294967295)::oid`
	var pid int
	if err := conn.pg.QueryRow(ctx, lockSession, controllerLockID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	ticks := time.NewTicker(100 * time.Millisecond)
	defer ticks.Stop()
	for {
		var probed bool
		err := conn.pg.QueryRow(ctx, `SELECT state='idle' AND query LIKE '%SELECT EXISTS (%'
			FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&probed)
		if err != nil {
			t.Fatal(err)
		}
		if probed {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("controller monitor stopped before session termination: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticks.C:
		}
	}
	var terminated bool
	if err := conn.pg.QueryRow(ctx, "SELECT pg_terminate_backend($1, 1000)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate lock session: ok=%v err=%v", terminated, err)
	}
	if err := <-result; err == nil {
		t.Fatal("Controller did not detect lock session loss")
	}
}
