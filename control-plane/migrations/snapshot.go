package migrations

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var snapshotSQL string

//go:embed schema.snapshot.json
var snapshotDescriptor []byte

type snapshot struct {
	Format         int    `json:"format"`
	Engine         string `json:"engine"`
	CoveredVersion int64  `json:"covered_version"`
	SchemaHash     string `json:"schema_sha256"`
	HistoryHash    string `json:"history_sha256"`
	SQL            string `json:"-"`
}

// historyDigest has an unambiguous encoding: numeric version, filename and
// lowercase raw-content SHA256 separated by tabs, one ordered record per line.
func historyDigest(known []Migration, through int64) string {
	h := sha256.New()
	for _, m := range known {
		if m.Version > through {
			break
		}
		fmt.Fprintf(h, "%d\t%s\t%x\n", m.Version, m.Name, m.Checksum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func loadSnapshot(known []Migration) (snapshot, error) {
	var s snapshot
	if err := json.Unmarshal(snapshotDescriptor, &s); err != nil {
		return s, fmt.Errorf("read snapshot descriptor: %w", err)
	}
	sum := sha256.Sum256([]byte(snapshotSQL))
	if len(known) == 0 || s.Format != 1 || s.Engine != "postgresql-18" || s.CoveredVersion != known[len(known)-1].Version || s.SchemaHash != hex.EncodeToString(sum[:]) || s.HistoryHash != historyDigest(known, s.CoveredVersion) {
		return s, errors.New("PostgreSQL snapshot does not match current migration history or schema.sql")
	}
	s.SQL = snapshotSQL
	if _, _, err := baselineArtifact(s.SQL); err != nil {
		return s, err
	}
	return s, nil
}

func (s snapshot) receipt() [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("1\tpostgresql-18\t%d\t%s\t%s\n", s.CoveredVersion, s.HistoryHash, s.SchemaHash)))
}

// Classification is read-only and runs under the same lock as initialization.
// public, system schemas and the default plpgsql extension may preexist.
// Namespace dependencies also cover collations, operators, conversions and
// text-search objects, without assuming that every user object is a relation.
// Database-global objects need separate catalog checks. These catalogs remain
// readable by a normal database owner, including when no superuser is used.
func databaseState(ctx context.Context, db queryer) (hasHistory, empty bool, err error) {
	err = db.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL,
 NOT (EXISTS(SELECT 1 FROM pg_namespace WHERE nspname <> 'public' AND nspname <> 'information_schema' AND nspname !~ '^pg_')
 OR EXISTS(SELECT 1 FROM pg_depend d JOIN pg_namespace n ON d.refclassid='pg_namespace'::regclass AND d.refobjid=n.oid WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_')
 OR EXISTS(SELECT 1 FROM pg_extension WHERE extname <> 'plpgsql')
 OR EXISTS(SELECT 1 FROM pg_event_trigger)
 OR EXISTS(SELECT 1 FROM pg_foreign_data_wrapper)
 OR EXISTS(SELECT 1 FROM pg_foreign_server)
 OR EXISTS(SELECT 1 FROM pg_publication)
 OR EXISTS(SELECT oid FROM pg_subscription WHERE subdbid=(SELECT oid FROM pg_database WHERE datname=current_database()))
 OR EXISTS(SELECT 1 FROM pg_largeobject_metadata)
 OR EXISTS(SELECT 1 FROM pg_default_acl)
 OR EXISTS(SELECT 1 FROM pg_depend WHERE classid IN ('pg_cast'::regclass,'pg_transform'::regclass))
 OR EXISTS(SELECT 1 FROM pg_language WHERE lanname NOT IN ('internal','c','sql','plpgsql')))
`).Scan(&hasHistory, &empty)
	return
}

func initializeSnapshot(ctx context.Context, conn *pgxpool.Conn, s snapshot, known []Migration) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer rollbackMigration(tx)
	if _, err = tx.Exec(ctx, s.SQL); err != nil {
		return fmt.Errorf("initialize PostgreSQL snapshot: %w", err)
	}
	// Coverage rows preserve the existing read contract. The origin explicitly
	// identifies their meaning; no historical data backfill is claimed executed.
	for _, m := range known {
		if m.Version > s.CoveredVersion {
			break
		}
		if _, err = tx.Exec(ctx, "INSERT INTO public.schema_migrations(version,name,checksum,snapshot_covered) VALUES($1,$2,$3,true)", m.Version, m.Name, m.Checksum[:]); err != nil {
			return err
		}
	}
	history, _ := hex.DecodeString(s.HistoryHash)
	schema, _ := hex.DecodeString(s.SchemaHash)
	receipt := s.receipt()
	if _, err = tx.Exec(ctx, `INSERT INTO public.schema_snapshot_origin(singleton,covered_version,history_sha256,schema_sha256,receipt_sha256) VALUES(true,$1,$2,$3,$4)`, s.CoveredVersion, history, schema, receipt[:]); err != nil {
		return err
	}
	if err := stampCheckpointOn(ctx, tx, s, known); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateOrigin(ctx context.Context, db queryer, known []Migration, applied []appliedMigration) error {
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.schema_snapshot_origin') IS NOT NULL").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		for _, m := range applied {
			if m.Version == 40 {
				return errors.New("snapshot provenance metadata missing after migration 40")
			}
		}
		return nil
	}
	marked := map[int64]bool{}
	rows, err := db.Query(ctx, "SELECT version FROM public.schema_migrations WHERE snapshot_covered")
	if err != nil {
		return err
	}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return err
		}
		marked[version] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var s snapshot
	var history, schema, receipt []byte
	err = db.QueryRow(ctx, `SELECT covered_version,history_sha256,schema_sha256,receipt_sha256 FROM public.schema_snapshot_origin WHERE singleton`).Scan(&s.CoveredVersion, &history, &schema, &receipt)
	if errors.Is(err, pgx.ErrNoRows) {
		if len(marked) != 0 {
			return errors.New("snapshot coverage rows have no provenance receipt")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read snapshot origin: %w", err)
	}
	s.HistoryHash = hex.EncodeToString(history)
	s.SchemaHash = hex.EncodeToString(schema)
	expected := s.receipt()
	if len(schema) != sha256.Size || s.HistoryHash != historyDigest(known, s.CoveredVersion) || !equalChecksum(receipt, expected[:]) {
		return errors.New("snapshot coverage receipt does not match historical source")
	}
	covered := make(map[int64]appliedMigration, len(applied))
	for _, m := range applied {
		covered[m.Version] = m
	}
	found := false
	expectedCovered := 0
	for _, m := range known {
		if m.Version > s.CoveredVersion {
			break
		}
		expectedCovered++
		if m.Version == s.CoveredVersion {
			found = true
		}
		row, ok := covered[m.Version]
		if !ok || !marked[m.Version] || row.Name != m.Name || !equalChecksum(row.Checksum, m.Checksum[:]) {
			return fmt.Errorf("snapshot coverage missing or altered migration %d", m.Version)
		}
	}
	if len(marked) != expectedCovered {
		return errors.New("snapshot coverage markers do not match provenance")
	}
	if !found {
		return errors.New("snapshot coverage has no known historical anchor")
	}
	return nil
}
