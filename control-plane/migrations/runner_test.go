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

func TestValidateAppliedMigrationsRejectsGap(t *testing.T) {
	first := testMigration(1, "000001_foundation.up.sql", "one")
	second := testMigration(2, "000002_next.up.sql", "two")

	err := validateAppliedMigrations([]Migration{first, second}, []appliedMigration{{
		Version: second.Version, Name: second.Name, Checksum: second.Checksum[:],
	}})
	if err == nil || !strings.Contains(err.Error(), "ordered prefix") {
		t.Fatalf("validateAppliedMigrations() error = %v, want migration-gap error", err)
	}
}

func TestSchemaCompatibilityAllowsOnlyDeclaredRange(t *testing.T) {
	tests := []struct {
		name     string
		contract SchemaCompatibility
		expected int64
		allowed  bool
	}{
		{name: "exact", contract: SchemaCompatibility{CurrentSchema: 29, MinimumCompatibleControllerSchema: 29}, expected: 29, allowed: true},
		{name: "declared older controller", contract: SchemaCompatibility{CurrentSchema: 30, MinimumCompatibleControllerSchema: 29}, expected: 29, allowed: true},
		{name: "too old", contract: SchemaCompatibility{CurrentSchema: 30, MinimumCompatibleControllerSchema: 29}, expected: 28, allowed: false},
		{name: "too new", contract: SchemaCompatibility{CurrentSchema: 29, MinimumCompatibleControllerSchema: 29}, expected: 30, allowed: false},
		{name: "invalid range", contract: SchemaCompatibility{CurrentSchema: 29, MinimumCompatibleControllerSchema: 30}, expected: 29, allowed: false},
		{name: "zero expected", contract: SchemaCompatibility{CurrentSchema: 29, MinimumCompatibleControllerSchema: 29}, expected: 0, allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.contract.Allows(test.expected); got != test.allowed {
				t.Fatalf("Allows(%d) = %v, want %v", test.expected, got, test.allowed)
			}
			if test.name == "invalid range" {
				if err := test.contract.Validate(); err == nil {
					t.Fatal("invalid range was accepted")
				}
			}
		})
	}
}

func TestValidateControllerAppliedMigrationsAllowsDeclaredFutureSuffix(t *testing.T) {
	known := []Migration{testMigration(1, "000001_foundation.up.sql", "one"), testMigration(2, "000002_next.up.sql", "two")}
	future := testMigration(3, "000003_future.up.sql", "three")
	applied := []appliedMigration{
		{Version: 1, Name: known[0].Name, Checksum: known[0].Checksum[:]},
		{Version: 2, Name: known[1].Name, Checksum: known[1].Checksum[:]},
		{Version: future.Version, Name: future.Name, Checksum: future.Checksum[:]},
	}
	if err := validateControllerAppliedMigrations(known, applied); err != nil {
		t.Fatalf("validateControllerAppliedMigrations() error = %v", err)
	}
}

func TestValidateControllerAppliedMigrationsRejectsMissingKnownMigration(t *testing.T) {
	known := []Migration{testMigration(1, "000001_foundation.up.sql", "one"), testMigration(2, "000002_next.up.sql", "two")}
	applied := []appliedMigration{{Version: 1, Name: known[0].Name, Checksum: known[0].Checksum[:]}}
	if err := validateControllerAppliedMigrations(known, applied); err == nil || !strings.Contains(err.Error(), "database schema is behind") {
		t.Fatalf("validateControllerAppliedMigrations() error = %v, want database-behind error", err)
	}
}

func testMigration(version int64, name, contents string) Migration {
	return Migration{Version: version, Name: name, Checksum: sha256.Sum256([]byte(contents))}
}
