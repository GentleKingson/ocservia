package mysql

import (
	"context"
	"errors"
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
		if schema, err := runtime.ControllerSchema(bounded, 34); err != nil || schema != 34 {
			t.Fatalf("runtime schema: %d %v", schema, err)
		}
	}
	ready()
	for _, expected := range []int64{0, 33, 35, 1 << 40} {
		if _, err := runtime.ControllerSchema(ctx, expected); !errors.Is(err, ErrSchema) {
			t.Fatalf("incompatible Controller %d: %v", expected, err)
		}
	}
	for _, change := range []struct{ mutate, restore string }{
		{`UPDATE backend_migrations SET dirty=true`, `UPDATE backend_migrations SET dirty=false`},
		{`UPDATE controller_schema_compatibility SET current_schema=35,minimum_compatible_controller_schema=35`, `UPDATE controller_schema_compatibility SET current_schema=34,minimum_compatible_controller_schema=34`},
		{`UPDATE backend_schema_revisions SET state='running' WHERE version=(SELECT MAX(version) FROM backend_schema_revision_steps)`, `UPDATE backend_schema_revisions SET state='verified' WHERE state='running'`},
		{`UPDATE backend_schema_revision_steps SET ordinal=100001 WHERE version=3 AND ordinal=1`, `UPDATE backend_schema_revision_steps SET ordinal=1 WHERE version=3 AND ordinal=100001`},
	} {
		if n, err := owner.Exec(ctx, change.mutate); err != nil || n != 1 {
			t.Fatalf("alter schema fixture: %d %v", n, err)
		}
		_, checkErr := runtime.ControllerSchema(ctx, 34)
		if _, err := owner.Exec(ctx, change.restore); err != nil {
			t.Fatal(err)
		}
		if checkErr == nil {
			t.Fatalf("runtime accepted altered schema ledger: %s", change.mutate)
		}
		ready()
	}
	var checksum string
	if err := owner.QueryRow(ctx, `SELECT checksum FROM backend_schema_revision_steps WHERE version=3 AND ordinal=1`).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE backend_schema_revision_steps SET checksum=REPEAT('0',64) WHERE version=3 AND ordinal=1`); err != nil {
		t.Fatal(err)
	}
	_, checkErr := runtime.ControllerSchema(ctx, 34)
	if _, err := owner.Exec(ctx, `UPDATE backend_schema_revision_steps SET checksum=? WHERE version=3 AND ordinal=1`, checksum); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkErr, ErrChecksum) {
		t.Fatalf("altered predecessor checksum: %v", checkErr)
	}
	ready()

	conn, name, err := migrationConnection(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, checkErr = runtime.ControllerSchema(bounded, 34)
	cancel()
	if err := releaseMigrationConnection(conn, name); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkErr, context.DeadlineExceeded) {
		t.Fatalf("migration lock wait ignored readiness deadline: %v", checkErr)
	}
	ready()
	if err := owner.ValidateSchema(ctx, 34); err != nil {
		t.Fatal("owner physical validation", err)
	}
}
