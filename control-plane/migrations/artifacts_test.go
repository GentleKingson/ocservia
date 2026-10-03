package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
	"github.com/jackc/pgx/v5/pgxpool"
)

func usePostgresArtifacts(t *testing.T, schema string, upgrade []byte) {
	t.Helper()
	originalSchema, originalUpgrade := snapshotSQL, upgradeSQL
	t.Cleanup(func() { snapshotSQL, upgradeSQL = originalSchema, originalUpgrade })
	snapshotSQL, upgradeSQL = schema, upgrade
}

func futurePostgresArtifacts(t *testing.T) (string, []byte, string) {
	t.Helper()
	ctx := context.Background()
	reference := snapshotDatabase(t)
	if err := Migrate(ctx, reference); err != nil {
		t.Fatal(err)
	}
	statement := "ALTER TABLE public.workspaces ADD COLUMN artifact_probe text;\n"
	if _, err := reference.Exec(ctx, statement); err != nil {
		t.Fatal(err)
	}
	var catalog string
	if err := reference.QueryRow(ctx, staticFingerprintSQL).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(catalog))
	after := fmt.Sprintf("%x", sum)
	_, before, err := baselineArtifact(snapshotSQL)
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.Replace(snapshotSQL, "-- ocservia:revision=0", "-- ocservia:revision=1", 1)
	schema = strings.Replace(schema, before, after, 1)
	schema = strings.Replace(schema, "-- ocservia:end-step\n", statement+"-- ocservia:end-step\n", 1)
	checkpoint, _ := json.Marshal(schemaartifact.Receipt{Checksum: fmt.Sprintf("%x", sha256.Sum256([]byte(schema))), Steps: 1})
	upgrade := string(upgradeSQL) + "\n-- ocservia:revision=1\n-- ocservia:checkpoint=" + string(checkpoint) + "\n-- ocservia:step=001:probe\n-- ocservia:metadata={\"catalog_sha256\":\"" + after + "\"}\n" + statement + "-- ocservia:end-step\n-- ocservia:end-revision\n"
	parsed, err := schemaartifact.Parse([]byte(upgrade), "postgresql")
	if err != nil {
		t.Fatal(err)
	}
	history, err := schemaartifact.HistoryChecksum(parsed, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldHash := fmt.Sprintf("%x", sha256.Sum256([]byte(schema)))
	schema = strings.Replace(schema, "-- ocservia:revision=1\n", "-- ocservia:revision=1\n-- ocservia:history-sha256="+history+"\n", 1)
	upgrade = strings.Replace(upgrade, oldHash, fmt.Sprintf("%x", sha256.Sum256([]byte(schema))), 1)
	return schema, []byte(upgrade), after
}

func postgresDatabaseDigest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var catalog, receipts string
	if err := pool.QueryRow(ctx, staticFingerprintSQL).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY epoch,revision),'[]')::text FROM public.schema_revisions r").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	return catalog + receipts
}

func TestPostgreSQLArtifactFreshAndUpgrade(t *testing.T) {
	ctx := context.Background()
	upgraded := snapshotDatabase(t)
	if err := Migrate(ctx, upgraded); err != nil {
		t.Fatal(err)
	}
	schema, upgrade, after := futurePostgresArtifacts(t)
	usePostgresArtifacts(t, schema, upgrade)
	// Fresh and checkpoint paths do not read legacy SQL or JSON at all.
	originalHistory, originalDescriptor := migrationFiles, snapshotDescriptor
	migrationFiles, snapshotDescriptor = embed.FS{}, []byte("invalid legacy descriptor")
	t.Cleanup(func() { migrationFiles, snapshotDescriptor = originalHistory, originalDescriptor })
	for range 2 {
		if err := Migrate(ctx, upgraded); err != nil {
			t.Fatal(err)
		}
	}
	fresh := snapshotDatabase(t)
	for range 2 {
		if err := Migrate(ctx, fresh); err != nil {
			t.Fatal(err)
		}
	}
	for _, pool := range []*pgxpool.Pool{fresh, upgraded} {
		if err := validateStaticSchema(ctx, pool, after); err != nil {
			t.Fatal(err)
		}
		var old int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&old); err != nil || old != 0 {
			t.Fatal("fabricated legacy history", old, err)
		}
	}
	freshRows, err := readRevisionReceipts(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	upgradeRows, err := readRevisionReceipts(ctx, upgraded)
	if err != nil {
		t.Fatal(err)
	}
	if len(freshRows) != 1 || freshRows[0].revision != 1 || len(upgradeRows) != 2 || upgradeRows[1].revision != 1 {
		t.Fatal("incorrect checkpoint/execution receipts", freshRows, upgradeRows)
	}
}

func TestPostgreSQLArtifactUnsupportedNoMutation(t *testing.T) {
	ctx := context.Background()
	for _, tamper := range []string{
		"UPDATE schema_revisions SET epoch=3",
		"UPDATE schema_revisions SET revision=42",
		"UPDATE schema_revisions SET checksum=decode(repeat('00',32),'hex')",
		"UPDATE schema_revisions SET state='running',verified_at=NULL",
		"UPDATE schema_revisions SET step=0",
		"INSERT INTO schema_revisions(epoch,revision,checksum,state,step,verified_at) SELECT epoch,2,checksum,state,step,verified_at FROM schema_revisions",
	} {
		t.Run(tamper, func(t *testing.T) {
			pool := snapshotDatabase(t)
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			before := postgresDatabaseDigest(t, pool)
			if err := Migrate(ctx, pool); err == nil {
				t.Fatal("unsupported journal accepted")
			}
			if after := postgresDatabaseDigest(t, pool); after != before {
				t.Fatal("refusal mutated schema/journal")
			}
		})
	}
}

func TestPostgreSQLArtifactRollback(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	schema, upgrade, _ := futurePostgresArtifacts(t)
	upgrade = []byte(strings.Replace(string(upgrade), "-- ocservia:end-step\n", "SELECT 1/0;\n-- ocservia:end-step\n", 1))
	// This is a new unpublished failing revision: keep its covered-history
	// provenance internally consistent so execution reaches the rollback case.
	a, err := schemaartifact.Parse(upgrade, "postgresql")
	if err != nil {
		t.Fatal(err)
	}
	history, err := schemaartifact.HistoryChecksum(a, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldSchemaHash := fmt.Sprintf("%x", sha256.Sum256([]byte(schema)))
	oldParsed, _ := schemaartifact.Parse([]byte(schema), "postgresql")
	schema = strings.Replace(schema, oldParsed.HistoryChecksum, history, 1)
	upgrade = []byte(strings.Replace(string(upgrade), oldSchemaHash, fmt.Sprintf("%x", sha256.Sum256([]byte(schema))), 1))
	usePostgresArtifacts(t, schema, upgrade)
	before := postgresDatabaseDigest(t, pool)
	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("failed revision committed")
	}
	if after := postgresDatabaseDigest(t, pool); after != before {
		t.Fatal("revision SQL/journal did not roll back")
	}
}

func TestPostgreSQLArtifactMalformedNoMutation(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	usePostgresArtifacts(t, snapshotSQL, append(append([]byte(nil), upgradeSQL...), []byte("-- ocservia:revision=2\n")...))
	before := postgresDatabaseDigest(t, pool)
	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("malformed artifact accepted")
	}
	if after := postgresDatabaseDigest(t, pool); after != before {
		t.Fatal("malformed artifact mutated database")
	}
}

func TestPostgreSQLArtifactHistoryMutationNoMutation(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	schema, upgrade, _ := futurePostgresArtifacts(t)
	upgrade = []byte(strings.Replace(string(upgrade), "ADD COLUMN artifact_probe", "ADD  COLUMN artifact_probe", 1))
	usePostgresArtifacts(t, schema, upgrade)
	before := postgresDatabaseDigest(t, pool)
	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("mutated covered revision accepted")
	}
	if after := postgresDatabaseDigest(t, pool); after != before {
		t.Fatal("history mutation changed database")
	}
}

func TestPostgreSQLArtifactPreviousCheckpoint(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	original, err := parsePostgresArtifacts([]byte(snapshotSQL), upgradeSQL)
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.Replace(snapshotSQL, "-- ocservia:epoch=1", "-- ocservia:epoch=2", 1)
	base := *original.upgrade.Base
	base.Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(schema)))
	baseline, _ := json.Marshal(base)
	previous, _ := json.Marshal(original.upgrade.Base)
	upgrade := "-- ocservia:artifact=upgrade\n-- ocservia:format=1\n-- ocservia:engine=postgresql\n-- ocservia:epoch=2\n-- ocservia:baseline=" + string(baseline) + "\n-- ocservia:previous-checkpoint-epoch=1\n-- ocservia:previous-checkpoint-revision=0\n-- ocservia:previous-checkpoint-ref=test-only-checkpoint\n-- ocservia:previous-checkpoint-receipt=" + string(previous) + "\n-- ocservia:transition=1:0->2:0\n-- ocservia:step=001:transition\nSELECT 1;\n-- ocservia:end-step\n-- ocservia:end-transition\n"
	usePostgresArtifacts(t, schema, []byte(upgrade))
	for range 2 {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := readRevisionReceipts(ctx, pool)
	if err != nil || len(rows) != 2 || rows[1].epoch != 2 || rows[1].revision != 0 {
		t.Fatal("checkpoint transition", rows, err)
	}
}
