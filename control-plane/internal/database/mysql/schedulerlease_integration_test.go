package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func schedulerFixture(t *testing.T) *Backend {
	t.Helper()
	b, _, _ := migrateFixture(t)
	return b
}

func TestRealSchedulerRunnerTransactions(t *testing.T) {
	owner := schedulerFixture(t)
	ctx := context.Background()
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	options := testOptions(t)
	config, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRow(ctx, `SELECT DATABASE()`).Scan(&config.DBName); err != nil {
		t.Fatal(err)
	}
	config.User, config.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = config.FormatDSN()
	b, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, sql := range []string{`INSERT INTO schema_revisions(epoch) VALUES(2)`, `UPDATE schema_revisions SET state='verified'`, `DELETE FROM schema_revisions`} {
		if _, err := b.Exec(ctx, sql); !errors.Is(err, database.ErrPermission) {
			t.Fatal("runtime can mutate migration receipts", err)
		}
	}
	var seed value.Timestamp
	if err := b.QueryRow(ctx, `SELECT lease_until FROM scheduler_leadership WHERE id=1`).Scan(&seed); err != nil || seed.Micros != value.NegativeInfinity {
		t.Fatal("published unacquired seed", seed, err)
	}
	first, err := coordination.AcquireBackend(ctx, b, coordination.Identity{InstanceID: uuid.New(), Incarnation: 1}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordination.AcquireBackend(ctx, b, coordination.Identity{InstanceID: uuid.New(), Incarnation: 2}, time.Second); !errors.Is(err, coordination.ErrLeaseHeld) {
		t.Fatal("live lease not exclusive", err)
	}
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := first.AssertTransaction(ctx, tx); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("expired lease passed long-transaction fence", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := coordination.AcquireBackend(ctx, b, coordination.Identity{InstanceID: uuid.New(), Incarnation: 3}, time.Second)
	if err != nil || second.Epoch() <= first.Epoch() {
		t.Fatal("takeover epoch", err)
	}
	if err := first.RenewBackend(ctx, b); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("stale renewal", err)
	}
	if err := second.RenewBackend(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error { return coordination.AssertFenceTx(ctx, tx, second) }); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `UPDATE scheduler_leadership SET lease_until=? WHERE id=1`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}); err != nil {
		t.Fatal(err)
	}
	runner := coordination.NewRunnerBackend(b, coordination.Identity{InstanceID: uuid.New(), Incarnation: 4}, time.Second, 50*time.Millisecond, nil)
	defer runner.Stop()
	if err := runner.WithSession(ctx, func(sessionCtx context.Context, s *coordination.Session) error {
		time.Sleep(130 * time.Millisecond)
		return database.Within(sessionCtx, b, database.ReadCommitted, func(tx database.Tx) error { return coordination.AssertFenceTx(sessionCtx, tx, s) })
	}); err != nil {
		t.Fatal("actual runner acquire/renew/fenced work", err)
	}
}
