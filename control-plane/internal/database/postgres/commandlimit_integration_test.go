package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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
	seedNode := func(t *testing.T, workspace uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$4)`, id, workspace, id.String(), now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seedScope := func(t *testing.T) (uuid.UUID, uuid.UUID) {
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
		return workspace, seedNode(t, workspace)
	}
	queueOps := func(ctx context.Context, tx database.Tx, workspace, node uuid.UUID, count int, state string) error {
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
	}
	seedCommand := func(ctx context.Context, tx database.Tx, workspace, node uuid.UUID, state string, lease bool) error {
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
	}
	t.Run("active_union_and_limit", func(t *testing.T) {
		workspace, node := seedScope(t)
		other := seedNode(t, workspace)
		tx, err := b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		before, err := commandlimit.Available(ctx, tx, 10000)
		if err != nil {
			t.Fatal(err)
		}
		if err = seedCommand(ctx, tx, workspace, node, "running", true); err != nil {
			t.Fatal(err)
		}
		if err = seedCommand(ctx, tx, workspace, other, "queued", true); err != nil {
			t.Fatal(err)
		}
		after, err := commandlimit.Available(ctx, tx, 10000)
		if err != nil || after != before-2 {
			t.Fatal("lease/active union double-counted or omitted", before, after, err)
		}
		for _, limit := range []int{-1, 0, 1} {
			if slots, err := commandlimit.Available(ctx, tx, limit); err != nil || slots != 0 {
				t.Fatal("active limit bypassed", limit, slots, err)
			}
		}
	})
	t.Run("quiescent_unknown_not_active", func(t *testing.T) {
		workspace, node := seedScope(t)
		other, third := seedNode(t, workspace), seedNode(t, workspace)
		tx, err := b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		before, err := commandlimit.Available(ctx, tx, 10000)
		if err != nil {
			t.Fatal(err)
		}
		if err = seedCommand(ctx, tx, workspace, node, "unknown", false); err != nil {
			t.Fatal(err)
		}
		if after, err := commandlimit.Available(ctx, tx, 10000); err != nil || after != before {
			t.Fatal("quiescent unknown consumed a global slot", before, after, err)
		}
		if err = seedCommand(ctx, tx, workspace, other, "unknown", true); err != nil {
			t.Fatal(err)
		}
		if after, err := commandlimit.Available(ctx, tx, 10000); err != nil || after != before-1 {
			t.Fatal("leased unknown not counted once", before, after, err)
		}
		if err = seedCommand(ctx, tx, workspace, third, "running", false); err != nil {
			t.Fatal(err)
		}
		if after, err := commandlimit.Available(ctx, tx, 10000); err != nil || after != before-2 {
			t.Fatal("running command stopped counting as active", before, after, err)
		}
	})
	for _, state := range []string{"dispatched", "accepted", "running", "unknown"} {
		for _, workspaceLimit := range []bool{false, true} {
			name, maximum := "backlog_counts_"+state+"_node", commandlimit.MaxNodeBacklog
			if workspaceLimit {
				name, maximum = "backlog_counts_"+state+"_workspace", commandlimit.MaxWorkspaceBacklog
			}
			t.Run(name, func(t *testing.T) {
				workspace, node := seedScope(t)
				queuedNode := node
				if workspaceLimit {
					queuedNode = uuid.Nil
				}
				tx, err := b.Begin(ctx, database.ReadCommitted)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if err = queueOps(ctx, tx, workspace, queuedNode, maximum-1, "queued"); err != nil {
					t.Fatal(err)
				}
				if err = commandlimit.ReserveBacklog(ctx, tx, workspace, node); err != nil {
					t.Fatal(err)
				}
				if err = queueOps(ctx, tx, workspace, queuedNode, 1, state); err != nil {
					t.Fatal(err)
				}
				if err = commandlimit.ReserveBacklog(ctx, tx, workspace, node); !errors.Is(err, commandlimit.ErrBacklogExceeded) {
					t.Fatal("unresolved command escaped the backlog ceiling", err)
				}
			})
		}
	}
	for _, workspaceLimit := range []bool{false, true} {
		name, maximum := "node_backlog", commandlimit.MaxNodeBacklog
		if workspaceLimit {
			name, maximum = "workspace_backlog", commandlimit.MaxWorkspaceBacklog
		}
		t.Run(name, func(t *testing.T) {
			workspace, node := seedScope(t)
			queuedNode := node
			if workspaceLimit {
				queuedNode = uuid.Nil
			}
			tx, err := b.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err = queueOps(ctx, tx, workspace, queuedNode, maximum-1, "queued"); err != nil {
				t.Fatal(err)
			}
			if err = queueOps(ctx, tx, workspace, queuedNode, 1, "succeeded"); err != nil {
				t.Fatal(err)
			}
			if err = commandlimit.ReserveBacklog(ctx, tx, workspace, node); err != nil {
				t.Fatal("terminal command counted as backlog", err)
			}
			if err = queueOps(ctx, tx, workspace, queuedNode, 1, "offline_pending"); err != nil {
				t.Fatal(err)
			}
			if err = commandlimit.ReserveBacklog(ctx, tx, workspace, node); !errors.Is(err, commandlimit.ErrBacklogExceeded) {
				t.Fatal("backlog ceiling bypassed", err)
			}
		})
	}
	for _, commit := range []bool{false, true} {
		name := "admission_lock_until_rollback"
		if commit {
			name = "admission_lock_until_commit"
		}
		t.Run(name, func(t *testing.T) {
			workspace, node := seedScope(t)
			first, err := b.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(ctx)
			before, err := commandlimit.Available(ctx, first, 10000)
			if err != nil {
				t.Fatal(err)
			}
			if err = seedCommand(ctx, first, workspace, node, "queued", true); err != nil {
				t.Fatal(err)
			}
			want := before
			if commit {
				want--
			}
			started := make(chan struct{})
			done := make(chan error, 1)
			waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			go func() {
				close(started)
				done <- database.Within(waiting, b, database.ReadCommitted, func(tx database.Tx) error {
					after, err := commandlimit.Available(waiting, tx, 10000)
					if err == nil && after != want {
						err = errors.New("committed/rolled-back reservation count changed")
					}
					return err
				})
			}()
			<-started
			select {
			case err := <-done:
				t.Fatal("admission lock released before transaction end", err)
			case <-time.After(100 * time.Millisecond):
			}
			if commit {
				err = first.Commit(ctx)
			} else {
				err = first.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("transaction end did not release admission lock")
			}
		})
	}
	t.Run("backlog_cancel_then_reuse", func(t *testing.T) {
		workspace, node := seedScope(t)
		first, err := b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback(ctx)
		if err = commandlimit.ReserveBacklog(ctx, first, workspace, node); err != nil {
			t.Fatal(err)
		}
		limited, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		err = database.Within(limited, b, database.ReadCommitted, func(tx database.Tx) error {
			return commandlimit.ReserveBacklog(limited, tx, workspace, node)
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("blocked admission ignored request cancellation", err)
		}
		if err = first.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		reuse, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if err = database.Within(reuse, b, database.ReadCommitted, func(tx database.Tx) error {
			return commandlimit.ReserveBacklog(reuse, tx, workspace, node)
		}); err != nil {
			t.Fatal("cancelled transaction leaked backlog lock", err)
		}
	})
}
