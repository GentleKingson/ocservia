package mysql

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func assertRuntimeDiagnostics(t *testing.T, owner, runtime *Backend) {
	t.Helper()
	ctx := context.Background()
	ready := func() {
		t.Helper()
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := runtime.CheckReadiness(bounded); err != nil {
			t.Fatalf("runtime connectivity: %v", err)
		}
	}
	ready()
	var checksum string
	var progress int
	var verified time.Time
	if err := owner.QueryRow(ctx, "SELECT checksum,step,verified_at FROM schema_revisions WHERE epoch=1 AND revision=0").Scan(&checksum, &progress, &verified); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		t.Helper()
		if _, err := owner.Exec(ctx, "UPDATE schema_revisions SET checksum=?,state='verified',step=?,verified_at=? WHERE epoch=1 AND revision=0", checksum, progress, verified); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"UPDATE schema_revisions SET state='running',verified_at=NULL WHERE epoch=1 AND revision=0", fmt.Sprintf("UPDATE schema_revisions SET step=%d WHERE epoch=1 AND revision=0", progress+1), "UPDATE schema_revisions SET checksum=REPEAT('0',64) WHERE epoch=1 AND revision=0"} {
		if n, err := owner.Exec(ctx, query); err != nil || n != 1 {
			t.Fatal("alter schema fixture", n, err)
		}
		err := owner.ValidateSchema(ctx)
		restore()
		if err == nil {
			t.Fatal("owner accepted altered journal", query)
		}
		ready()
	}
	conn, name, err := migrationConnection(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	checkErr := owner.ValidateSchema(bounded)
	cancel()
	if err := releaseMigrationConnection(conn, name); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkErr, context.DeadlineExceeded) {
		t.Fatalf("migration lock wait ignored validation deadline: %v", checkErr)
	}
	ready()
	if err := owner.ValidateSchema(ctx); err != nil {
		t.Fatal("owner physical validation", err)
	}
}
