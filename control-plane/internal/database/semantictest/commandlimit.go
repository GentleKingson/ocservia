package semantictest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type CommandLimitHarness struct {
	Backend database.Backend
	Scope   func(*testing.T) (uuid.UUID, uuid.UUID)
	Node    func(*testing.T, uuid.UUID) uuid.UUID
	Queue   func(context.Context, database.Tx, uuid.UUID, uuid.UUID, int, string) error
	Command func(context.Context, database.Tx, uuid.UUID, uuid.UUID, string, bool) error
}

// CommandLimits exercises the real admission API and its transaction lifetime,
// with identical cases for the backend-owned SQL implementations.
func CommandLimits(t *testing.T, h CommandLimitHarness) {
	t.Helper()
	ctx := context.Background()
	t.Run("active_union_and_limit", func(t *testing.T) {
		workspace, node := h.Scope(t)
		other := h.Node(t, workspace)
		tx, err := h.Backend.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		before, err := commandlimit.Available(ctx, tx, 10000)
		if err != nil {
			t.Fatal(err)
		}
		if err = h.Command(ctx, tx, workspace, node, "running", true); err != nil {
			t.Fatal(err)
		}
		if err = h.Command(ctx, tx, workspace, other, "queued", true); err != nil {
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
	for _, workspaceLimit := range []bool{false, true} {
		name, maximum := "node_backlog", commandlimit.MaxNodeBacklog
		if workspaceLimit {
			name, maximum = "workspace_backlog", commandlimit.MaxWorkspaceBacklog
		}
		t.Run(name, func(t *testing.T) {
			workspace, node := h.Scope(t)
			queuedNode := node
			if workspaceLimit {
				queuedNode = uuid.Nil
			}
			tx, err := h.Backend.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err = h.Queue(ctx, tx, workspace, queuedNode, maximum-1, "queued"); err != nil {
				t.Fatal(err)
			}
			if err = h.Queue(ctx, tx, workspace, queuedNode, 1, "succeeded"); err != nil {
				t.Fatal(err)
			}
			if err = commandlimit.ReserveBacklog(ctx, tx, workspace, node); err != nil {
				t.Fatal("terminal command counted as backlog", err)
			}
			if err = h.Queue(ctx, tx, workspace, queuedNode, 1, "offline_pending"); err != nil {
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
			workspace, node := h.Scope(t)
			first, err := h.Backend.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(ctx)
			before, err := commandlimit.Available(ctx, first, 10000)
			if err != nil {
				t.Fatal(err)
			}
			if err = h.Command(ctx, first, workspace, node, "queued", true); err != nil {
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
				done <- database.Within(waiting, h.Backend, database.ReadCommitted, func(tx database.Tx) error {
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
		workspace, node := h.Scope(t)
		first, err := h.Backend.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback(ctx)
		if err = commandlimit.ReserveBacklog(ctx, first, workspace, node); err != nil {
			t.Fatal(err)
		}
		limited, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		err = database.Within(limited, h.Backend, database.ReadCommitted, func(tx database.Tx) error {
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
		if err = database.Within(reuse, h.Backend, database.ReadCommitted, func(tx database.Tx) error {
			return commandlimit.ReserveBacklog(reuse, tx, workspace, node)
		}); err != nil {
			t.Fatal("cancelled transaction leaked backlog lock", err)
		}
	})
}
