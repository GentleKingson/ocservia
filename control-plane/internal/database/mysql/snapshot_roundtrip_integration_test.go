package mysql

import (
	"context"
	"strings"
	"testing"
)

// mysqldump uses SHOW CREATE verbatim, unlike the generator which omits
// inherited column COLLATE clauses. Replay every table's real SHOW output in
// dependency order and verify it still has the immutable schema fingerprint.
func TestSnapshotDumpTableRoundtrip(t *testing.T) {
	ctx := context.Background()
	source, _, _ := migrateFixture(t)
	target, _, _ := fixture(t)
	original, err := source.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	restored, err := target.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	a, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	identityHash := ""
	for _, statement := range a.schema.Baseline.Steps {
		m := a.meta(statement)
		if m.Kind != "table" {
			continue
		}
		name := m.Object
		var returned, ddl, after string
		if err = original.QueryRowContext(ctx, "SHOW CREATE TABLE `"+name+"`").Scan(&returned, &ddl); err != nil {
			t.Fatal(err)
		}
		if _, err = restored.ExecContext(ctx, ddl); err != nil {
			t.Fatal(name, safeError(err))
		}
		if err = restored.QueryRowContext(ctx, "SHOW CREATE TABLE `"+name+"`").Scan(&returned, &after); err != nil {
			t.Fatal(err)
		}
		expected, err := schemaHash(ctx, original, step{Name: name, Kind: "table"})
		if err != nil {
			t.Fatal(err)
		}
		actual, err := schemaHash(ctx, restored, step{Name: name, Kind: "table"})
		if err != nil || actual != expected {
			t.Fatalf("%s source=%s restored=%s: %v", name, expected, actual, err)
		}
		if name == "identities" {
			identityHash = expected
			if strings.Contains(ddl, "CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin") || !strings.Contains(after, "CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin") {
				t.Fatal("fixture did not exercise inherited-charset SHOW CREATE drift")
			}
			t.Logf("identities raw source=%s restored=%s; canonical=%s", digest([]byte(ddl)), digest([]byte(after)), actual)
		}
	}
	for _, query := range []string{
		"ALTER TABLE identities MODIFY issuer LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL",
		"ALTER TABLE identities MODIFY issuer LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL, DROP CHECK identities_chk_1",
	} {
		if _, err = restored.ExecContext(ctx, query); err != nil {
			t.Fatal(safeError(err))
		}
		actual, err := schemaHash(ctx, restored, step{Name: "identities", Kind: "table"})
		if err != nil {
			t.Fatal(err)
		}
		if actual == identityHash {
			t.Fatal("semantic collation/constraint drift normalized away")
		}
	}
}
