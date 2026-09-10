package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCommandLimitsIntegration(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	b := WrapPool(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	node := func(t *testing.T, workspace uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$4)`, id, workspace, id.String(), now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	semantictest.CommandLimits(t, semantictest.CommandLimitHarness{
		Backend: b,
		Scope: func(t *testing.T) (uuid.UUID, uuid.UUID) {
			workspace := uuid.New()
			if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'command-limit',$2,$3,$3)`, workspace, workspace.String(), now); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, table := range []string{"commands", "operations", "nodes"} {
					if _, err := b.Exec(ctx, "DELETE FROM "+table+" WHERE workspace_id=$1", workspace); err != nil {
						t.Error(err)
					}
				}
				if _, err := b.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspace); err != nil {
					t.Error(err)
				}
			})
			return workspace, node(t, workspace)
		},
		Node: node,
		Queue: func(ctx context.Context, tx database.Tx, workspace, node uuid.UUID, count int, state string) error {
			var nodeValue any
			if node != uuid.Nil {
				nodeValue = node
			}
			for range count {
				if _, err := tx.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at) VALUES($1,$2,$3,$4,'command-limit',$5,$5)`, uuid.New(), workspace, nodeValue, state, now); err != nil {
					return err
				}
			}
			return nil
		},
		Command: func(ctx context.Context, tx database.Tx, workspace, node uuid.UUID, state string, lease bool) error {
			operation, command := uuid.New(), uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at) VALUES($1,$2,$3,$4,'command-limit',$5,$5)`, operation, workspace, node, state, now); err != nil {
				return err
			}
			trace := "00-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01"
			if _, err := tx.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'synthetic_noop',$6,$7,1,$8,$9,$10,$10)`, command, operation, workspace, node, state, []byte("x"), command.String(), trace, now.Add(time.Minute), now); err != nil {
				return err
			}
			if lease {
				_, err := tx.Exec(ctx, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at) VALUES($1,$2,$3,$4,$5,$6)`, node, command, uuid.New(), uuid.New(), now.Add(time.Minute), now)
				return err
			}
			return nil
		},
	})
}
