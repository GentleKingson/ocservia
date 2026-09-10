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
	for _, sql := range []string{`SELECT * FROM time_migration_decisions`, `INSERT INTO time_migration_decisions VALUES('scheduler_leadership','lease_until','1','1000-01-01','finite')`, `UPDATE time_migration_decisions SET decision='finite'`, `DELETE FROM time_migration_decisions`} {
		if _, err := b.Exec(ctx, sql); !errors.Is(err, database.ErrPermission) {
			t.Fatal("runtime can access migration decisions", err)
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

func TestRealSchedulerAmbiguousSentinelDecision(t *testing.T) {
	for _, decision := range []string{"finite", "negative_infinity"} {
		t.Run(decision, func(t *testing.T) {
			b, _ := versionTwoFixture(t, false)
			ctx := context.Background()
			chain, err := loadRevisionChain(b.engine)
			if err != nil {
				t.Fatal(err)
			}
			conn, lock, err := migrationConnection(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			err = b.migrateChainOn(ctx, conn, chain[:2], "")
			unlock := releaseMigrationConnection(conn, lock)
			if err != nil || unlock != nil {
				t.Fatal(err, unlock)
			}
			id := uuid.New()
			if _, err := b.Exec(ctx, `UPDATE scheduler_leadership SET instance_id=?,incarnation=3,epoch=9 WHERE id=1`, id[:]); err != nil {
				t.Fatal(err)
			}
			if err := b.Migrate(ctx, ""); !errors.Is(err, ErrSchema) {
				t.Fatalf("ambiguous sentinel migrated: %v", err)
			}
			if err := b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
				t.Fatalf("dirty revision resumed implicitly: %v", err)
			}
			if _, err := b.Exec(ctx, `UPDATE scheduler_leadership SET epoch=10 WHERE id=1`); err == nil {
				t.Fatal("owner bypassed migration writer guard")
			}
			conn, name, err := migrationConnection(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			_, err = conn.ExecContext(ctx, `INSERT INTO time_migration_decisions VALUES('scheduler_leadership','lease_until','1','1000-01-01 00:00:00',?)`, decision)
			unlock = releaseMigrationConnection(conn, name)
			if err != nil || unlock != nil {
				t.Fatal(err, unlock)
			}
			if err = b.Migrate(ctx, chain[len(chain)-1].sum); err != nil {
				t.Fatal(err)
			}
			var got value.Timestamp
			if err := b.QueryRow(ctx, `SELECT lease_until FROM scheduler_leadership WHERE id=1`).Scan(&got.Micros); err != nil {
				t.Fatal(err)
			}
			want := int64(value.NegativeInfinity)
			if decision == "finite" {
				finite, err := value.FromTime(time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatal(err)
				}
				want = finite.Micros
			}
			if got.Micros != want {
				t.Fatal("owner decision lost", got.Micros, want)
			}
		})
	}
}
