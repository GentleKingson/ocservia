package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
)

func TestMySQLBridgeCheckpoint(t *testing.T) {
	ctx := context.Background()
	b, _, _ := migrateFixture(t)
	chain, err := loadRevisionChain(MySQL)
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentSnapshot(chain)
	if err != nil {
		t.Fatal(err)
	}
	a, err := schemaartifact.Parse(current.sql, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateBridgeFromBackend(ctx, b, chain); err != nil {
		t.Fatal(err)
	}
	var count, step int
	if err := b.QueryRow(ctx, "SELECT COUNT(*),MAX(step) FROM schema_revisions").Scan(&count, &step); err != nil || count != 1 || step != len(a.Baseline.Steps) {
		t.Fatal("incorrect checkpoint", count, step, err)
	}
	for _, table := range []string{"backend_migrations", "backend_migration_steps", "backend_schema_revisions", "backend_schema_revision_steps"} {
		if err := b.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatal("fabricated historical execution", table, count, err)
		}
	}
	for _, tamper := range []string{
		"UPDATE schema_revisions SET checksum=REPEAT('0',64)",
		"UPDATE schema_revisions SET epoch=2",
		"UPDATE schema_revisions SET revision=1",
		"UPDATE schema_revisions SET step=0",
		"UPDATE schema_revisions SET state='running',verified_at=NULL",
		"UPDATE schema_revisions SET verified_at=started_at-INTERVAL 1 SECOND",
	} {
		t.Run(tamper, func(t *testing.T) {
			other, _, _ := migrateFixture(t)
			if _, err := other.Exec(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if err := other.Migrate(ctx, ""); !errors.Is(err, ErrChecksum) {
				t.Fatal("checkpoint tamper accepted", err)
			}
			if err := other.ValidateSchema(ctx); !errors.Is(err, ErrChecksum) {
				t.Fatal("validation ignored checkpoint", err)
			}
		})
	}
}

func validateBridgeFromBackend(ctx context.Context, b *Backend, chain []revisionArtifact) error {
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return validateBridgeJournal(ctx, conn, chain)
}

func TestMySQLBridgeDoesNotStampUnverifiedLegacy(t *testing.T) {
	ctx := context.Background()
	b, _ := historicalFixture(t, false)
	chain, err := loadRevisionChain(MySQL)
	if err != nil {
		t.Fatal(err)
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.migrateChainOn(ctx, conn, chain, ""); err != nil {
		t.Fatal(err)
	}
	if err = releaseMigrationConnection(conn, lock); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, "ALTER TABLE workspaces ADD COLUMN foreign_column INT"); err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, ""); !errors.Is(err, ErrSchema) {
		t.Fatal("unverified legacy schema accepted", err)
	}
	var count int
	if err = b.QueryRow(ctx, "SELECT COUNT(*) FROM schema_revisions").Scan(&count); err != nil || count != 0 {
		t.Fatal("checkpoint stamped before validation", count, err)
	}
}
