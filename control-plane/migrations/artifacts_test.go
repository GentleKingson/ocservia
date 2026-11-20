package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
	"github.com/jackc/pgx/v5/pgxpool"
)

func usePostgresArtifacts(t *testing.T, schema string, upgrade []byte) {
	t.Helper()
	originalSchema, originalUpgrade := schemaSQL, upgradeSQL
	t.Cleanup(func() { schemaSQL, upgradeSQL = originalSchema, originalUpgrade })
	schemaSQL, upgradeSQL = schema, upgrade
}

func futurePostgresArtifacts(t *testing.T) (string, []byte, string) {
	t.Helper()
	ctx := context.Background()
	reference := snapshotDatabase(t)
	testutil.NoError(t, Migrate(ctx, reference))
	statement := "ALTER TABLE public.workspaces ADD COLUMN artifact_probe text;\n"
	if _, err := reference.Exec(ctx, statement); err != nil {
		t.Fatal(err)
	}
	var catalog string
	testutil.NoError(t, reference.QueryRow(ctx, staticFingerprintSQL).Scan(&catalog))
	sum := sha256.Sum256([]byte(catalog))
	after := fmt.Sprintf("%x", sum)
	original, err := parsePostgresArtifacts([]byte(schemaSQL), upgradeSQL)
	before, _ := original.catalog(original.schema.Baseline.Number)
	testutil.NoError(t, err)
	current, next := original.schema.Baseline.Number, original.schema.Baseline.Number+1
	schema := strings.Replace(schemaSQL, fmt.Sprintf("-- ocservia:revision=%d", current), fmt.Sprintf("-- ocservia:revision=%d", next), 1)
	schema = strings.Replace(schema, before, after, 1)
	schema = strings.Replace(schema, "-- ocservia:end-step\n", statement+"-- ocservia:end-step\n", 1)
	checkpoint, _ := json.Marshal(schemaartifact.Receipt{Checksum: fmt.Sprintf("%x", sha256.Sum256([]byte(schema))), Steps: 1})
	upgrade := string(upgradeSQL) + fmt.Sprintf("\n-- ocservia:revision=%d\n-- ocservia:checkpoint=", next) + string(checkpoint) + "\n-- ocservia:step=001:probe\n-- ocservia:metadata={\"catalog_sha256\":\"" + after + "\"}\n" + statement + "-- ocservia:end-step\n-- ocservia:end-revision\n"
	parsed, err := schemaartifact.Parse([]byte(upgrade), "postgresql")
	testutil.NoError(t, err)
	history, err := schemaartifact.HistoryChecksum(parsed, next)
	testutil.NoError(t, err)
	oldHash := fmt.Sprintf("%x", sha256.Sum256([]byte(schema)))
	schema = strings.Replace(schema, original.schema.HistoryChecksum, history, 1)
	upgrade = strings.Replace(upgrade, oldHash, fmt.Sprintf("%x", sha256.Sum256([]byte(schema))), 1)
	return schema, []byte(upgrade), after
}

func postgresDatabaseDigest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var catalog, receipts string
	testutil.NoError(t, pool.QueryRow(ctx, staticFingerprintSQL).Scan(&catalog))
	testutil.NoError(t, pool.QueryRow(ctx, "SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY epoch,revision),'[]')::text FROM public.schema_revisions r").Scan(&receipts))
	return catalog + receipts
}

func TestPostgreSQLArtifactFreshAndUpgrade(t *testing.T) {
	ctx := context.Background()
	original, err := parsePostgresArtifacts([]byte(schemaSQL), upgradeSQL)
	testutil.NoError(t, err)
	current := original.schema.Baseline.Number
	upgraded := snapshotDatabase(t)
	testutil.NoError(t, Migrate(ctx, upgraded))
	schema, upgrade, after := futurePostgresArtifacts(t)
	usePostgresArtifacts(t, schema, upgrade)
	for range 2 {
		testutil.NoError(t, Migrate(ctx, upgraded))
	}
	fresh := snapshotDatabase(t)
	for range 2 {
		testutil.NoError(t, Migrate(ctx, fresh))
	}
	for _, pool := range []*pgxpool.Pool{fresh, upgraded} {
		testutil.NoError(t, validateStaticSchema(ctx, pool, after))
		var old int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relnamespace='public'::regnamespace AND relname IN ('schema_migrations','schema_snapshot_origin')").Scan(&old); err != nil || old != 0 {
			t.Fatal("fabricated legacy history", old, err)
		}
	}
	freshRows, err := readRevisionReceipts(ctx, fresh)
	testutil.NoError(t, err)
	upgradeRows, err := readRevisionReceipts(ctx, upgraded)
	testutil.NoError(t, err)
	if len(freshRows) != 1 || freshRows[0].revision != current+1 || len(upgradeRows) != 2 || upgradeRows[0].revision != current || upgradeRows[1].revision != current+1 {
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
		"INSERT INTO schema_revisions(epoch,revision,checksum,state,step,verified_at) SELECT epoch,revision+1,checksum,state,step,verified_at FROM schema_revisions",
	} {
		t.Run(tamper, func(t *testing.T) {
			pool := snapshotDatabase(t)
			testutil.NoError(t, Migrate(ctx, pool))
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
	testutil.NoError(t, Migrate(ctx, pool))
	schema, upgrade, _ := futurePostgresArtifacts(t)
	upgrade = []byte(strings.Replace(string(upgrade), "ADD COLUMN artifact_probe text;\n", "ADD COLUMN artifact_probe text;\nSELECT 1/0;\n", 1))
	// This is a new unpublished failing revision: keep its covered-history
	// provenance internally consistent so execution reaches the rollback case.
	a, err := schemaartifact.Parse(upgrade, "postgresql")
	testutil.NoError(t, err)
	history, err := schemaartifact.HistoryChecksum(a, int64(len(a.Revisions)))
	testutil.NoError(t, err)
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
	testutil.NoError(t, Migrate(ctx, pool))
	usePostgresArtifacts(t, schemaSQL, append(append([]byte(nil), upgradeSQL...), []byte("-- ocservia:revision=2\n")...))
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
	testutil.NoError(t, Migrate(ctx, pool))
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

func TestPostgreSQLArtifactCatalog(t *testing.T) {
	a, err := parsePostgresArtifacts([]byte(schemaSQL), upgradeSQL)
	testutil.NoError(t, err)
	if a.schema.Epoch != 2 || a.schema.Baseline.Number != 5 || a.upgrade.Previous == nil || a.upgrade.Previous.Ref != checkpointRef {
		t.Fatal("major cutover must use the released immutable checkpoint")
	}
	for _, metadata := range []string{`{"catalog_sha256":"` + strings.Repeat("a", 64) + `","unknown":1}`, `{"catalog_sha256":"` + strings.Repeat("a", 64) + `","catalog_sha256":"` + strings.Repeat("a", 64) + `"}`, `{"catalog_sha256":"` + strings.Repeat("A", 64) + `"}`} {
		if _, err := catalogMetadata([]byte(metadata)); err == nil {
			t.Fatal("noncanonical fingerprint metadata accepted")
		}
	}
}
