package migrations

import (
	"crypto/sha256"
	"testing"
)

func TestValidateAppliedMigrationsRejectsAlteredKnownContent(t *testing.T) {
	known := testMigration(1, "000001_foundation.up.sql", "one")
	for _, applied := range []appliedMigration{
		{Version: 1, Name: "changed.up.sql", Checksum: known.Checksum[:]},
		{Version: 1, Name: known.Name, Checksum: make([]byte, sha256.Size)},
	} {
		if err := validateAppliedMigrations([]Migration{known}, []appliedMigration{applied}); err == nil {
			t.Fatal("altered known migration accepted")
		}
	}
}

func TestValidateAppliedMigrationsIgnoresUnknownVersion(t *testing.T) {
	known := testMigration(1, "000001_foundation.up.sql", "one")
	unknown := testMigration(2, "000002_future.up.sql", "two")

	err := validateAppliedMigrations([]Migration{known}, []appliedMigration{{
		Version: unknown.Version, Name: unknown.Name, Checksum: unknown.Checksum[:],
	}})
	if err != nil {
		t.Fatalf("validateAppliedMigrations() error = %v, want no version-policy error", err)
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

func TestValidateAppliedMigrationsAllowsUnappliedKnownMigration(t *testing.T) {
	first := testMigration(1, "000001_foundation.up.sql", "one")
	second := testMigration(2, "000002_next.up.sql", "two")

	err := validateAppliedMigrations([]Migration{first, second}, []appliedMigration{{
		Version: second.Version, Name: second.Name, Checksum: second.Checksum[:],
	}})
	if err != nil {
		t.Fatalf("validateAppliedMigrations() error = %v, want missing migration to remain eligible for execution", err)
	}
}

func testMigration(version int64, name, contents string) Migration {
	return Migration{Version: version, Name: name, Checksum: sha256.Sum256([]byte(contents))}
}
