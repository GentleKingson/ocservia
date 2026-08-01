package migrations

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestValidateAppliedMigrationsRejectsUnknownVersion(t *testing.T) {
	known := testMigration(1, "000001_foundation.up.sql", "one")
	unknown := testMigration(2, "000002_future.up.sql", "two")

	err := validateAppliedMigrations([]Migration{known}, []appliedMigration{{
		Version: unknown.Version, Name: unknown.Name, Checksum: unknown.Checksum[:],
	}})
	if err == nil || !strings.Contains(err.Error(), "unknown to this binary") {
		t.Fatalf("validateAppliedMigrations() error = %v, want unknown-version error", err)
	}
}

func TestValidateAppliedMigrationsAcceptsKnownVersion(t *testing.T) {
	known := testMigration(1, "000001_foundation.up.sql", "one")
	err := validateAppliedMigrations([]Migration{known}, []appliedMigration{{
		Version: known.Version, Name: known.Name, Checksum: known.Checksum[:],
	}})
	if err != nil {
		t.Fatalf("validateAppliedMigrations() error = %v", err)
	}
}

func testMigration(version int64, name, contents string) Migration {
	return Migration{Version: version, Name: name, Checksum: sha256.Sum256([]byte(contents))}
}
