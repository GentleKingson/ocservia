package mysql

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// Exercise current initialization and identical retries, not a historical lineage.
func TestDatabaseInitializationSmoke(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	before := baselineReceipts(t, b)
	id := UUIDBytes(uuid.New())
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'initialization smoke','initialization-smoke',0,0)`, id); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := b.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=?`, id).Scan(&name); err != nil || name != "initialization smoke" {
		t.Fatal(name, err)
	}
	if !reflect.DeepEqual(before, baselineReceipts(t, b)) {
		t.Fatal("identical initialization rewrote receipts")
	}
}
