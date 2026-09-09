package mysql

import (
	"context"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealUsageTransactions(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	config, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	config.User, config.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = config.FormatDSN()
	b, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	semantictest.UsageTransactions(t, semantictest.UsageHarness{
		Backend: b,
		SeedNode: func(t *testing.T) uuid.UUID {
			workspace, node := uuid.New(), uuid.New()
			if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'usage',?,CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(workspace), workspace.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'usage','active',CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, UUIDBytes(node), UUIDBytes(workspace)); err != nil {
				t.Fatal(err)
			}
			return node
		},
		CursorCount: func(ctx context.Context, node uuid.UUID) (int, error) {
			var count int
			err := b.QueryRow(ctx, `SELECT count(*) FROM user_usage_cursors WHERE node_id=?`, UUIDBytes(node)).Scan(&count)
			return count, err
		},
		ReadTotals: func(ctx context.Context, node uuid.UUID) ([]semantictest.UsageTotal, error) {
			rows, err := b.Query(ctx, `SELECT period,period_start,rx_bytes,tx_bytes FROM observed_user_usage WHERE node_id=?`, UUIDBytes(node))
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
