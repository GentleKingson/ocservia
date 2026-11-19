// Package testutil provides assertions shared by Controller tests.
package testutil

import "testing"

func NoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
