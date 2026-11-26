package migrations

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestEmbeddedGooseMigrationsParseCleanly(t *testing.T) {
	fsys, err := migrationFS()
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	cfg, err := pgx.ParseConfig("postgres://127.0.0.1:5432/ocservia")
	if err != nil {
		t.Fatalf("parse dummy pgx config: %v", err)
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithAllowOutofOrder(false))
	if err != nil {
		t.Fatalf("parse embedded goose migrations: %v", err)
	}
	sources := provider.ListSources()
	if len(sources) != 1 {
		t.Fatalf("expected 1 embedded migration, got %d", len(sources))
	}
	if sources[0].Version != 1 {
		t.Fatalf("first migration version = %d, want 1", sources[0].Version)
	}
}
