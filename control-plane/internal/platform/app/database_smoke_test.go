package app

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

func checkSmokeTransactions(t *testing.T, ctx context.Context, store database.Backend, backend string, workspace any) {
	t.Helper()
	update, read := `UPDATE workspaces SET name=$1 WHERE id=$2`, `SELECT name FROM workspaces WHERE id=$1`
	if backend != "postgres" {
		update, read = `UPDATE workspaces SET name=? WHERE id=?`, `SELECT name FROM workspaces WHERE id=?`
	}
	rollback := errors.New("rollback smoke transaction")
	for _, commit := range []bool{true, false} {
		name := "committed workspace"
		if !commit {
			name = "rolled back workspace"
		}
		err := database.Within(ctx, store, database.ReadCommitted, func(tx database.Tx) error {
			if _, err := tx.Exec(ctx, update, name, workspace); err != nil {
				return err
			}
			var got string
			if err := tx.QueryRow(ctx, read, workspace).Scan(&got); err != nil {
				return err
			}
			if got != name {
				t.Fatalf("transaction read: %q", got)
			}
			// Store uses another pooled connection while the transaction is open.
			var outside string
			if err := store.QueryRow(ctx, read, workspace).Scan(&outside); err != nil {
				return err
			}
			if outside == name {
				t.Fatal("uncommitted workspace visible on another connection")
			}
			if !commit {
				return rollback
			}
			return nil
		})
		if commit && err != nil || !commit && !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		var got string
		if err := store.QueryRow(ctx, read, workspace).Scan(&got); err != nil || got != "committed workspace" {
			t.Fatal("commit/rollback readback", got, err)
		}
	}
}
