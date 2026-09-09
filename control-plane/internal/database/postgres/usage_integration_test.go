package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUsageTransactionsIntegration(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	b := WrapPool(pool)
	semantictest.UsageTransactions(t, semantictest.UsageHarness{
		Backend: b,
		SeedNode: func(t *testing.T) uuid.UUID {
			workspace, node := uuid.New(), uuid.New()
			if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'usage',$2,now(),now())`, workspace, workspace.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'usage','active',now(),now())`, node, workspace); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := b.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node); err != nil {
					t.Error(err)
				}
				if _, err := b.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspace); err != nil {
					t.Error(err)
				}
			})
			return node
		},
		CursorCount: func(ctx context.Context, node uuid.UUID) (int, error) {
			var count int
			err := b.QueryRow(ctx, `SELECT count(*) FROM user_usage_cursors WHERE node_id=$1`, node).Scan(&count)
			return count, err
		},
		ReadTotals: func(ctx context.Context, node uuid.UUID) ([]semantictest.UsageTotal, error) {
			rows, err := b.Query(ctx, `SELECT period,period_start,rx_bytes,tx_bytes FROM observed_user_usage WHERE node_id=$1`, node)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var totals []semantictest.UsageTotal
			for rows.Next() {
				var total semantictest.UsageTotal
				if err := rows.Scan(&total.Period, &total.Start, &total.RX, &total.TX); err != nil {
					return nil, err
				}
				totals = append(totals, total)
			}
			return totals, rows.Err()
		},
	})
}
