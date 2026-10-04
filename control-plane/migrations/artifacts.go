package migrations

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

//go:embed upgrade.sql
var upgradeSQL []byte

const checkpointRef = "v1.2.0@169102557cd610847c9f6ac2083336cdcf82c483"
const checkpointChecksum = "d837335f22c70858f4e2a5277332e478ecd6032e7b55f57d6512484bfab6f172"

type postgresArtifacts struct {
	schema, upgrade schemaartifact.Artifact
}

func catalogMetadata(data []byte) (string, error) {
	var m struct {
		CatalogSHA256 string `json:"catalog_sha256"`
	}
	if json.Unmarshal(data, &m) != nil {
		return "", errors.New("invalid PostgreSQL schema verification metadata")
	}
	decoded, err := hex.DecodeString(m.CatalogSHA256)
	canonical, _ := json.Marshal(m)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != m.CatalogSHA256 || !bytes.Equal(data, canonical) {
		return "", errors.New("missing PostgreSQL schema fingerprint")
	}
	return m.CatalogSHA256, nil
}

func parsePostgresArtifacts(schema, upgrade []byte) (postgresArtifacts, error) {
	var a postgresArtifacts
	var err error
	a.schema, err = schemaartifact.Parse(schema, "postgresql")
	if err != nil {
		return a, err
	}
	a.upgrade, err = schemaartifact.Parse(upgrade, "postgresql")
	if err != nil {
		return a, err
	}
	if a.schema.Kind != "schema" || a.upgrade.Kind != "upgrade" || a.schema.Epoch != a.upgrade.Epoch || a.schema.Baseline.Number != int64(len(a.upgrade.Revisions)) || a.upgrade.Base == nil || (a.schema.Epoch > 1 && a.upgrade.Previous == nil) {
		return a, errors.New("PostgreSQL artifact window mismatch")
	}
	if a.schema.Epoch > 1 && a.schema.Baseline.Number == 0 && a.schema.HistoryChecksum != fmt.Sprintf("%x", a.upgrade.Transition.Checksum) {
		return a, errors.New("PostgreSQL transition checksum mismatch")
	}
	if a.schema.Baseline.Number > 0 {
		history, err := schemaartifact.HistoryChecksum(a.upgrade, a.schema.Baseline.Number)
		if err != nil || a.schema.HistoryChecksum != history {
			return a, errors.New("PostgreSQL schema coverage checksum mismatch")
		}
	}
	fresh, err := catalogMetadata(a.schema.Baseline.Steps[len(a.schema.Baseline.Steps)-1].Metadata)
	if err != nil {
		return a, err
	}
	head, err := a.catalog(a.schema.Baseline.Number)
	if err != nil || fresh != head {
		return a, errors.New("PostgreSQL fresh/upgrade fingerprint mismatch")
	}
	checkpoint := a.upgrade.Base
	if a.schema.Baseline.Number > 0 {
		checkpoint = a.upgrade.Revisions[a.schema.Baseline.Number-1].Checkpoint
	}
	if checkpoint == nil || checkpoint.Checksum != fmt.Sprintf("%x", a.schema.Checksum) || checkpoint.Steps != len(a.schema.Baseline.Steps) {
		return a, errors.New("PostgreSQL current schema checkpoint mismatch")
	}
	for _, r := range a.upgrade.Revisions {
		if _, err := catalogMetadata(r.Steps[len(r.Steps)-1].Metadata); err != nil {
			return a, err
		}
	}
	if a.upgrade.Previous != nil {
		transition := a.upgrade.Transition
		if transition == nil || len(transition.Steps) < 2 || transition.Steps[len(transition.Steps)-1].Name != fmt.Sprintf("%03d:legacy_cleanup", len(transition.Steps)) {
			return a, errors.New("PostgreSQL transition requires a final legacy cleanup step")
		}
		final, err := catalogMetadata(transition.Steps[len(transition.Steps)-1].Metadata)
		baseline, _ := a.catalog(0)
		if err != nil || final != baseline {
			return a, errors.New("PostgreSQL transition/fresh fingerprint mismatch")
		}
		if a.upgrade.Previous.Receipt == nil || a.upgrade.Previous.Ref != checkpointRef || a.upgrade.Previous.Epoch != 1 || a.upgrade.Previous.Revision != 0 || a.upgrade.Previous.Receipt.Checksum != checkpointChecksum || a.upgrade.Previous.Receipt.Steps != 1 {
			return a, errors.New("previous checkpoint requires a pinned receipt")
		}
		if _, err := catalogMetadata(a.upgrade.Previous.Receipt.Metadata); err != nil {
			return a, err
		}
	}
	return a, nil
}

func (a postgresArtifacts) catalog(revision int64) (string, error) {
	if revision == 0 {
		return catalogMetadata(a.upgrade.Base.Metadata)
	}
	if revision < 0 || revision > int64(len(a.upgrade.Revisions)) {
		return "", errors.New("unsupported PostgreSQL revision")
	}
	r := a.upgrade.Revisions[revision-1]
	return catalogMetadata(r.Steps[len(r.Steps)-1].Metadata)
}

type revisionReceipt struct {
	epoch, revision int64
	checksum        []byte
	state           string
	step            int
	started         time.Time
	verified        *time.Time
}

func readRevisionReceipts(ctx context.Context, db queryer) ([]revisionReceipt, error) {
	rows, err := db.Query(ctx, "SELECT epoch,revision,checksum,state,step,started_at,verified_at FROM public.schema_revisions ORDER BY epoch,revision")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []revisionReceipt
	for rows.Next() {
		var r revisionReceipt
		if err := rows.Scan(&r.epoch, &r.revision, &r.checksum, &r.state, &r.step, &r.started, &r.verified); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func matchesReceipt(row revisionReceipt, sum string, steps int) bool {
	return row.state == "verified" && row.step == steps && row.verified != nil && !row.verified.Before(row.started) && hex.EncodeToString(row.checksum) == sum
}

func (a postgresArtifacts) admit(rows []revisionReceipt) (int64, bool, error) {
	if len(rows) == 0 {
		return 0, false, errors.New("empty PostgreSQL revision journal")
	}
	current := rows
	if rows[0].epoch != a.schema.Epoch {
		p := a.upgrade.Previous
		if p == nil || rows[0].epoch != p.Epoch || rows[0].revision != p.Revision || !matchesReceipt(rows[0], p.Receipt.Checksum, p.Receipt.Steps) {
			return 0, false, errors.New("unsupported PostgreSQL epoch/checkpoint")
		}
		current = rows[1:]
		if len(current) == 0 {
			return 0, true, nil
		}
	}
	var last int64
	retainedPrevious := len(current) != len(rows)
	for i, row := range current {
		if row.epoch != a.schema.Epoch || row.revision < 0 || row.revision > int64(len(a.upgrade.Revisions)) || (i > 0 && row.revision != last+1) {
			return 0, false, errors.New("unsupported or noncontiguous PostgreSQL journal")
		}
		var sum string
		var steps int
		if i == 0 && retainedPrevious {
			if row.revision != 0 {
				return 0, false, errors.New("missing PostgreSQL checkpoint transition")
			}
			sum, steps = a.upgrade.Base.Checksum, a.upgrade.Base.Steps
		} else if i == 0 {
			checkpoint := a.upgrade.Base
			if row.revision > 0 {
				checkpoint = a.upgrade.Revisions[row.revision-1].Checkpoint
			}
			if checkpoint == nil {
				return 0, false, errors.New("missing PostgreSQL checkpoint receipt")
			}
			sum, steps = checkpoint.Checksum, checkpoint.Steps
		} else {
			r := a.upgrade.Revisions[row.revision-1]
			sum, steps = fmt.Sprintf("%x", r.Checksum), len(r.Steps)
		}
		if !matchesReceipt(row, sum, steps) {
			return 0, false, errors.New("PostgreSQL revision checksum/state mismatch")
		}
		last = row.revision
	}
	return last, false, nil
}

func insertRevisionReceipt(ctx context.Context, tx pgx.Tx, epoch, revision int64, sum string, steps int) error {
	decoded, err := hex.DecodeString(sum)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.schema_revisions(epoch,revision,checksum,state,step,verified_at) VALUES($1,$2,$3,'verified',$4,now())`, epoch, revision, decoded, steps)
	return err
}

func initializeArtifact(ctx context.Context, conn *pgxpool.Conn, a postgresArtifacts) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer rollbackMigration(tx)
	for _, s := range a.schema.Baseline.Steps {
		if _, err = tx.Exec(ctx, string(s.SQL)); err != nil {
			return fmt.Errorf("initialize PostgreSQL schema: %w", err)
		}
	}
	if _, err = tx.Exec(ctx, "SET LOCAL search_path TO public"); err != nil {
		return err
	}
	expected, _ := a.catalog(a.schema.Baseline.Number)
	if err = validateStaticSchema(ctx, tx, expected); err != nil {
		return err
	}
	if err = insertRevisionReceipt(ctx, tx, a.schema.Epoch, a.schema.Baseline.Number, fmt.Sprintf("%x", a.schema.Checksum), len(a.schema.Baseline.Steps)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func applyArtifactRevision(ctx context.Context, conn *pgxpool.Conn, a postgresArtifacts, r schemaartifact.Revision, transition bool) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer rollbackMigration(tx)
	stepsToApply := r.Steps
	if transition {
		stepsToApply = r.Steps[:len(r.Steps)-1]
	}
	for _, s := range stepsToApply {
		if _, err = tx.Exec(ctx, string(s.SQL)); err != nil {
			return fmt.Errorf("PostgreSQL revision %d: %w", r.Number, err)
		}
	}
	if _, err = tx.Exec(ctx, "SET LOCAL search_path TO public"); err != nil {
		return err
	}
	expected, err := a.catalog(r.Number)
	if err != nil {
		return err
	}
	if err = validateRevisionSchema(ctx, tx, expected, transition); err != nil {
		return err
	}
	sum, steps := fmt.Sprintf("%x", r.Checksum), len(r.Steps)
	if transition {
		sum, steps = a.upgrade.Base.Checksum, a.upgrade.Base.Steps
	}
	if err = insertRevisionReceipt(ctx, tx, a.schema.Epoch, r.Number, sum, steps); err != nil {
		return err
	}
	if transition {
		rows, err := readRevisionReceipts(ctx, tx)
		if err != nil {
			return err
		}
		if _, pending, err := a.admit(rows); err != nil || pending {
			return errors.New("PostgreSQL new checkpoint was not verified before legacy cleanup")
		}
		if _, err := tx.Exec(ctx, string(r.Steps[len(r.Steps)-1].SQL)); err != nil {
			return err
		}
		if err := validateStaticSchema(ctx, tx, expected); err != nil {
			return err
		}
		rows, err = readRevisionReceipts(ctx, tx)
		if err != nil {
			return err
		}
		if _, pending, err := a.admit(rows); err != nil || pending || len(rows) != 1 {
			return errors.New("PostgreSQL new checkpoint changed during legacy cleanup")
		}
	}
	return tx.Commit(ctx)
}

func migrateArtifacts(ctx context.Context, pool *pgxpool.Pool) error {
	a, err := parsePostgresArtifacts([]byte(schemaSQL), upgradeSQL)
	if err != nil {
		return err
	}
	return withMigrationConnection(ctx, pool, func(conn *pgxpool.Conn) error {
		empty, err := databaseState(ctx, conn)
		if err != nil {
			return err
		}
		if empty {
			if err := initializeArtifact(ctx, conn, a); err != nil {
				return err
			}
		}
		var journal bool
		if err := conn.QueryRow(ctx, "SELECT to_regclass('public.schema_revisions') IS NOT NULL").Scan(&journal); err != nil {
			return err
		}
		var rows []revisionReceipt
		if journal {
			rows, err = readRevisionReceipts(ctx, conn)
			if err != nil {
				return err
			}
		}
		if len(rows) == 0 {
			return errors.New("nonempty PostgreSQL database has no trusted checkpoint; upgrade with the designated v1.2.0 bridge first")
		}
		head, transition, err := a.admit(rows)
		if err != nil {
			return err
		}
		if transition {
			previous, _ := catalogMetadata(a.upgrade.Previous.Receipt.Metadata)
			if err := validateStaticSchema(ctx, conn, previous); err != nil {
				return err
			}
			if err := applyArtifactRevision(ctx, conn, a, *a.upgrade.Transition, true); err != nil {
				return err
			}
		} else {
			expected, _ := a.catalog(head)
			if err := validateStaticSchema(ctx, conn, expected); err != nil {
				return err
			}
		}
		for _, r := range a.upgrade.Revisions {
			if r.Number > head {
				if err := applyArtifactRevision(ctx, conn, a, r, false); err != nil {
					return err
				}
			}
		}
		_, err = conn.Exec(ctx, `SELECT telemetry_ensure_month_partition(month AT TIME ZONE 'UTC') FROM generate_series(date_trunc('month',now() AT TIME ZONE 'UTC')-interval '1 month',date_trunc('month',now() AT TIME ZONE 'UTC')+interval '2 months',interval '1 month') AS month`)
		return err
	})
}
