package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
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
		"57P01": database.ErrUnavailable, "57P02": database.ErrUnavailable, "57P03": database.ErrUnavailable,
		"08003": database.ErrUnavailable, "08006": database.ErrUnavailable,
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

func TestUnavailableConnectionClassification(t *testing.T) {
	for _, original := range []error{io.EOF, io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}} {
		err := classify(fmt.Errorf("wrapped: %w", original))
		if !errors.Is(err, database.ErrUnavailable) || !errors.Is(err, original) {
			t.Fatalf("connection availability/original error lost: %v", err)
		}
	}
	for _, original := range []error{context.Canceled, context.DeadlineExceeded, &pgconn.PgError{Code: "42501"}, &pgconn.PgError{Code: "28P01"}, &pgconn.PgError{Code: "42P01"}, &pgconn.PgError{Code: "08P01"}} {
		if errors.Is(classify(original), database.ErrUnavailable) {
			t.Fatalf("non-availability failure classified as recoverable: %v", original)
		}
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
