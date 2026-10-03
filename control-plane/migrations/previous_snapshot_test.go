package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
	"github.com/jackc/pgx/v5"
)

func TestPostgreSQLPreviousSnapshotBridge(t *testing.T) {
	ctx := context.Background()
	pool := snapshotDatabase(t)
	known, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	a, err := schemaartifact.Parse([]byte(snapshotSQL), "postgresql")
	if err != nil {
		t.Fatal(err)
	}
	oldSQL := string(a.Baseline.Steps[0].SQL)
	// Reconstruct the actual pre-bridge SQL, then prove every byte against the
	// immutable main snapshot descriptor at c85687ea31e2d45208d09b49706e2ee7d324acdc.
	start := strings.Index(oldSQL, "CREATE TABLE public.schema_revisions (")
	if start < 0 {
		t.Fatal("journal DDL missing")
	}
	end := strings.Index(oldSQL[start:], ");\n\n")
	if end < 0 {
		t.Fatal("journal DDL boundary missing")
	}
	oldSQL = oldSQL[:start] + oldSQL[start+end+4:]
	oldSQL = strings.Replace(oldSQL, "ALTER TABLE ONLY public.schema_revisions\n    ADD CONSTRAINT schema_revisions_pkey PRIMARY KEY (epoch, revision);\n\n", "", 1)
	sum := sha256.Sum256([]byte(oldSQL))
	old := snapshot{Format: 1, Engine: "postgresql-18", CoveredVersion: 40, SchemaHash: "2a3ced6c7cd5265128b705a32a1b1cb7c82d6764326e330201078c91dafb25ba", HistoryHash: "8f6b9d5f40b7b1e7473b6872405b398684fdb25a4c33c6f364c49833aa98be09", SQL: oldSQL}
	if hex.EncodeToString(sum[:]) != old.SchemaHash || historyDigest(known, 40) != old.HistoryHash {
		t.Fatal("real previous snapshot bytes/history changed")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, oldSQL); err != nil {
		t.Fatal(err)
	}
	for _, m := range known {
		if m.Version > 40 {
			break
		}
		if _, err = tx.Exec(ctx, "INSERT INTO public.schema_migrations(version,name,checksum,snapshot_covered) VALUES($1,$2,$3,true)", m.Version, m.Name, m.Checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	history, _ := hex.DecodeString(old.HistoryHash)
	receipt := old.receipt()
	if _, err = tx.Exec(ctx, "INSERT INTO public.schema_snapshot_origin(singleton,covered_version,history_sha256,schema_sha256,receipt_sha256) VALUES(true,40,$1,$2,$3)", history, sum[:], receipt[:]); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var before string
	query := "SELECT to_jsonb(o)::text FROM public.schema_snapshot_origin o WHERE singleton"
	if err = pool.QueryRow(ctx, query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = Migrate(ctx, pool); err != nil {
			t.Fatal("previous snapshot bridge", err)
		}
	}
	var after string
	if err = pool.QueryRow(ctx, query).Scan(&after); err != nil || after != before {
		t.Fatal("origin rewritten", after, err)
	}
	var valid bool
	if err = pool.QueryRow(ctx, "SELECT (SELECT count(*)=40 FROM public.schema_migrations WHERE snapshot_covered) AND (SELECT count(*)=1 FROM public.schema_migrations WHERE NOT snapshot_covered AND version=41) AND (SELECT count(*)=1 AND bool_and(epoch=1 AND revision=0 AND state='verified' AND checksum=$1) FROM public.schema_revisions)", a.Checksum[:]).Scan(&valid); err != nil || !valid {
		t.Fatal(fmt.Sprintf("bridge receipts invalid: %v", err))
	}
}
