package mysql

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// Full CI checks one supported immutable lineage: revision 25 -> current.
// Earlier revisions, draft lineages and interrupted repairs remain manual.
func TestDatabaseUpgradeSmoke(t *testing.T) {
	b, _ := historicalFixture(t, false)
	ctx := context.Background()
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	err = b.migrateChainOn(ctx, conn, chain[:24], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	before := baselineReceipts(t, b)
	id := UUIDBytes(uuid.New())
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'upgrade smoke','upgrade-smoke',0,0)`, id); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateSchema(ctx, 36); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := b.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=?`, id).Scan(&name); err != nil || name != "upgrade smoke" {
		t.Fatal(name, err)
	}
	if !reflect.DeepEqual(before, baselineReceipts(t, b)) {
		t.Fatal("upgrade rewrote baseline receipts")
	}
}
