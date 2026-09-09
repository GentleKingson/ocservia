package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
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
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return WrapPool(pool)
}

func TestTransactionCancellationRollbackIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	for isolation, want := range map[database.Isolation]string{database.DefaultIsolation: "", database.ReadCommitted: "read committed", database.RepeatableRead: "repeatable read", database.Serializable: "serializable"} {
		err := database.Within(ctx, b, isolation, func(tx database.Tx) error {
			var level string
			if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&level); err != nil {
				return err
			}
			if want != "" && level != want {
				t.Fatalf("isolation %q != %q", level, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := tx.Exec(cancelled, "SELECT 1")
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err = database.Within(deadline, b, database.ReadCommitted, func(tx database.Tx) error {
		_, err := tx.Exec(deadline, "SELECT pg_sleep(5)")
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if err := b.pool.Ping(ctx); err != nil {
		t.Fatalf("pool after cleanup: %v", err)
	}
	tx, err := b.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
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
			sample := userusage.Sample{SessionID: "session", Username: "user", Connected: now, ObservedAt: now, RXBytes: 10, TXBytes: 20}
			if err := userusage.RecordTx(ctx, a, node, []userusage.Sample{sample}); err != nil {
				return err
			}
			prior, err := other.LockCursor(ctx, node, sample)
			if err != nil || prior.RXBytes != 10 {
				t.Fatalf("second store cannot see uncommitted cursor: %+v %v", prior, err)
			}
			sample.RXBytes = 15
			sample.ObservedAt = now.Add(time.Second)
			if err := userusage.RecordTx(ctx, other, node, []userusage.Sample{sample, sample}); err != nil {
				return err
			}
			var total int64
			if err := tx.QueryRow(ctx, "SELECT sum(rx_bytes) FROM observed_user_usage").Scan(&total); err != nil {
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

func TestLegacyTransactionBridgeIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	if _, err := b.Exec(ctx, "CREATE TEMP TABLE legacy_marker (id int PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	legacy, err := b.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacy.Rollback(ctx) }()
	if _, err := legacy.Exec(ctx, "INSERT INTO legacy_marker VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	wrapped := WrapTx(legacy)
	var count int
	if err := wrapped.QueryRow(ctx, "SELECT count(*) FROM legacy_marker").Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy write not visible: %d %v", count, err)
	}
	if _, err := wrapped.Exec(ctx, "INSERT INTO legacy_marker VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.QueryRow(ctx, "SELECT count(*) FROM legacy_marker").Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy rollback did not cover both stores: %d %v", count, err)
	}
}
