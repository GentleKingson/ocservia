package testutil

import (
	"context"
	"testing"
)

// Native pgx pools and Controller backends return different result types.
type executor[R any] interface {
	Exec(context.Context, string, ...any) (R, error)
}

// Exec runs fixture SQL when the test only needs the statement to succeed.
func Exec[R any](t testing.TB, ctx context.Context, db executor[R], query string, args ...any) {
	t.Helper()
	_, err := db.Exec(ctx, query, args...)
	NoError(t, err)
}
