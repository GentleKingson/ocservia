package postgres

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testBackend(t *testing.T) *Backend {
	t.Helper()
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	testutil.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	testutil.NoError(t, err)
	t.Cleanup(pool.Close)
	return WrapPool(pool)
}

func TestTransactionCancellationRollbackIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	for isolation, want := range map[database.Isolation]string{database.DefaultIsolation: "", database.ReadCommitted: "read committed", database.RepeatableRead: "repeatable read", database.Serializable: "serializable"} {
		err := database.Within(ctx, b, isolation, func(tx database.Tx) error {
			var level string
			if err := tx.(*transaction).QueryRow(ctx, "SHOW transaction_isolation").Scan(&level); err != nil {
				return err
			}
			if want != "" && level != want {
				t.Fatalf("isolation %q != %q", level, want)
			}
			return nil
		})
		testutil.NoError(t, err)
	}
	err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := tx.(*transaction).Exec(cancelled, "SELECT 1")
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err = database.Within(deadline, b, database.ReadCommitted, func(tx database.Tx) error {
		_, err := tx.(*transaction).Exec(deadline, "SELECT pg_sleep(5)")
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if err := b.pool.Ping(ctx); err != nil {
		t.Fatalf("pool after cleanup: %v", err)
	}
	tx, err := b.Begin(ctx, database.ReadCommitted)
	testutil.NoError(t, err)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(tx.Commit(ctx), database.ErrTxClosed) {
		t.Fatal("commit after rollback")
	}
}

func TestUsageCrossStoreTransactionIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	// One pooled connection keeps temporary fixtures isolated from real data.
	for _, sql := range []string{
		`CREATE TEMP TABLE user_usage_cursors (node_id uuid, session_id text, connected_at timestamptz, username text, rx_bytes bigint, tx_bytes bigint, observed_at timestamptz, PRIMARY KEY(node_id,session_id,connected_at))`,
		`CREATE TEMP TABLE observed_user_usage (node_id uuid, username text, period text, period_start timestamptz, rx_bytes bigint, tx_bytes bigint, observed_at timestamptz, PRIMARY KEY(node_id,username,period,period_start))`,
	} {
		if _, err := b.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, rollback := range []bool{true, false} {
		sentinel := errors.New("business failure")
		err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			a, other := NewUsageStore(tx), NewUsageStore(tx)
			node := uuid.New()
			now := time.Now().UTC().Truncate(time.Microsecond)
			sample := userstore.UsageSample{SessionID: "session", Username: "user", Connected: now, ObservedAt: now, RXBytes: 10, TXBytes: 20}
			if err := userstore.RecordUsageTx(ctx, a, node, []userstore.UsageSample{sample}); err != nil {
				return err
			}
			key := userstore.UsageKey{SessionID: sample.SessionID, Connected: now}
			cursors, err := other.LockCursors(ctx, node, []userstore.UsageKey{key})
			if err != nil || cursors[key].RXBytes != 10 {
				t.Fatalf("second store cannot see uncommitted cursor: %+v %v", cursors[key], err)
			}
			sample.RXBytes = 15
			sample.ObservedAt = now.Add(time.Second)
			if err := userstore.RecordUsageTx(ctx, other, node, []userstore.UsageSample{sample, sample}); err != nil {
				return err
			}
			var total int64
			if err := tx.(*transaction).QueryRow(ctx, "SELECT sum(rx_bytes) FROM observed_user_usage").Scan(&total); err != nil {
				return err
			}
			if total != 30 {
				t.Fatalf("replay/delta total: %d", total)
			}
			if rollback {
				return sentinel
			}
			return nil
		})
		if rollback && !errors.Is(err, sentinel) || !rollback && err != nil {
			t.Fatal(err)
		}
		var cursors, totals int
		if err := b.QueryRow(ctx, "SELECT (SELECT count(*) FROM user_usage_cursors),(SELECT count(*) FROM observed_user_usage)").Scan(&cursors, &totals); err != nil {
			t.Fatal(err)
		}
		if rollback && (cursors != 0 || totals != 0) || !rollback && (cursors != 1 || totals != 2) {
			t.Fatalf("atomicity: cursors=%d totals=%d", cursors, totals)
		}
	}
}

func TestServerErrorClassificationIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	for code, want := range map[string]error{"23505": database.ErrUnique, "23503": database.ErrForeignKey, "40001": database.ErrSerialization, "40P01": database.ErrDeadlock, "42501": database.ErrPermission} {
		_, err := b.Exec(ctx, "DO $$ BEGIN RAISE EXCEPTION USING ERRCODE='"+code+"'; END $$")
		if !errors.Is(err, want) {
			t.Fatalf("want %v: %v", want, err)
		}
	}
	var v int
	if err := b.QueryRow(ctx, "SELECT 1 WHERE false").Scan(&v); !errors.Is(err, database.ErrNotFound) {
		t.Fatal(err)
	}
}
