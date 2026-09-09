package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestErrorClassification(t *testing.T) {
	for code, want := range map[string]error{
		"23505": database.ErrUnique, "23503": database.ErrForeignKey,
		"23502": database.ErrConstraint, "23514": database.ErrConstraint, "23P01": database.ErrConstraint,
		"40001": database.ErrSerialization, "40P01": database.ErrDeadlock,
		"42501": database.ErrPermission, "25P02": database.ErrTxAborted,
	} {
		t.Run(code, func(t *testing.T) {
			original := &pgconn.PgError{Code: code}
			err := classify(fmt.Errorf("wrapped: %w", original))
			if !errors.Is(err, want) || !errors.Is(err, original) {
				t.Fatalf("classification: %v", err)
			}
		})
	}
	for _, test := range []struct{ source, want error }{
		{pgx.ErrNoRows, database.ErrNotFound}, {pgx.ErrTxClosed, database.ErrTxClosed},
		{pgx.ErrTxCommitRollback, database.ErrTxAborted},
		{context.Canceled, context.Canceled}, {context.DeadlineExceeded, context.DeadlineExceeded},
	} {
		if !errors.Is(classify(test.source), test.want) {
			t.Fatalf("classification: %v", test.source)
		}
	}
	unknown := &pgconn.PgError{Code: "57014"}
	if classify(unknown) != unknown || classify(nil) != nil {
		t.Fatal("unknown errors must not be guessed or retried")
	}
}

func TestCapabilitiesAndIsolationFailClosed(t *testing.T) {
	b := WrapPool(nil)
	if err := database.Require(b, database.Transactions, database.RowLocks); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(database.Require(b, "unknown"), database.ErrUnsupported) {
		t.Fatal("unknown capability accepted")
	}
	if _, err := b.Begin(context.Background(), database.Isolation(255)); !errors.Is(err, database.ErrUnsupported) {
		t.Fatal(err)
	}
}
