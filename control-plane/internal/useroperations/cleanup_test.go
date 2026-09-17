package useroperations

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
)

type cleanupBackend struct {
	database.Backend
	tx       *cleanupTransaction
	beginErr error
}

func (b cleanupBackend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	b.tx.ctx, b.tx.isolation = ctx, isolation
	return b.tx, b.beginErr
}

type cleanupTransaction struct {
	database.Tx
	userstore.Store
	ctx                    context.Context
	isolation              database.Isolation
	deleted                userstore.Candidate
	checkSource, committed bool
	rolledBack             bool
	deleteErr, commitErr   error
}

func (tx *cleanupTransaction) UserOperationsStore() userstore.Store { return tx }
func (tx *cleanupTransaction) DeleteEnforcement(ctx context.Context, item userstore.Candidate, check bool) error {
	if ctx != tx.ctx {
		panic("cleanup changed request context")
	}
	tx.deleted, tx.checkSource = item, check
	return tx.deleteErr
}
func (tx *cleanupTransaction) Commit(ctx context.Context) error {
	if ctx != tx.ctx {
		panic("cleanup commit changed request context")
	}
	tx.committed = true
	return tx.commitErr
}
func (tx *cleanupTransaction) Rollback(context.Context) error {
	tx.rolledBack = true
	return nil
}

func TestEnforcementCleanupTransactionErrors(t *testing.T) {
	for _, stage := range []string{"success", "begin", "delete", "commit", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New("injected transaction failure")
			tx := &cleanupTransaction{}
			backend := cleanupBackend{tx: tx}
			switch stage {
			case "begin":
				backend.beginErr = failure
			case "delete":
				tx.deleteErr = failure
			case "commit":
				tx.commitErr = failure
			case "cancel":
				failure, tx.deleteErr = context.Canceled, context.Canceled
			}
			item := userstore.Candidate{NodeID: uuid.New(), Username: "alice", PolicyVersion: 3, Cause: "quota"}
			err := NewBackend(backend, nil).cleanupEnforcement(t.Context(), item, true)
			if stage == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var cleanup *EnforcementCleanupError
				if !errors.As(err, &cleanup) || !errors.Is(err, failure) || cleanup.NodeID != item.NodeID || cleanup.Username != item.Username || cleanup.PolicyVersion != item.PolicyVersion || cleanup.Cause != item.Cause {
					t.Fatalf("lost cleanup identity or cause: %v", err)
				}
			}
			if tx.isolation != database.ReadCommitted || tx.rolledBack != (stage != "begin") || tx.committed != (stage == "success" || stage == "commit") {
				t.Fatalf("transaction boundary changed: %+v", tx)
			}
			if stage != "begin" && (tx.deleted != item || !tx.checkSource) {
				t.Fatal("cleanup changed predicates")
			}
		})
	}
}
