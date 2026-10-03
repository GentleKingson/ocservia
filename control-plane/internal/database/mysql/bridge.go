package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
)

const journalDDL = `CREATE TABLE schema_revisions (
 epoch BIGINT NOT NULL CHECK(epoch>0),
 revision BIGINT NOT NULL CHECK(revision>=0),
 checksum VARBINARY(64) NOT NULL CHECK(OCTET_LENGTH(checksum)=64),
 state VARBINARY(16) NOT NULL CHECK(state IN ('running','verified')),
 step INT NOT NULL CHECK(step>=0),
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 verified_at DATETIME(6) NULL,
 PRIMARY KEY(epoch,revision),
 CHECK((state='verified')=(verified_at IS NOT NULL))
) ENGINE=InnoDB`

type artifactStepMetadata struct {
	Kind           string   `json:"kind"`
	Object         string   `json:"object"`
	Before         string   `json:"before"`
	After          string   `json:"after"`
	VerifySQL      string   `json:"verify_sql,omitempty"`
	CheckBeforeSQL string   `json:"check_before_sql,omitempty"`
	Repairable     bool     `json:"repairable,omitempty"`
	RoundtripHash  string   `json:"roundtrip_hash,omitempty"`
	Columns        []string `json:"columns,omitempty"`
}

func validateCheckpointWindow(ctx context.Context, conn *sql.Conn, chain []revisionArtifact) error {
	actual, err := schemaHash(ctx, conn, step{Name: "backend_schema_revisions", Kind: "table"})
	if err != nil {
		return err
	}
	if actual == "" {
		return nil
	}
	if actual != chain[0].MetadataHashes["backend_schema_revisions"] {
		return ErrSchema
	}
	var unknown int
	err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM backend_schema_revisions WHERE version<2 OR version>?", chain[len(chain)-1].Version).Scan(&unknown)
	if err != nil {
		return safeError(err)
	}
	if unknown != 0 {
		var running int
		err = conn.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM backend_schema_revisions WHERE version>? AND state='running')+(SELECT COUNT(*) FROM backend_schema_revision_steps WHERE version>? AND state='running')", chain[len(chain)-1].Version, chain[len(chain)-1].Version).Scan(&running)
		if err != nil {
			return safeError(err)
		}
		if running != 0 {
			return ErrDirty
		}
		return ErrChecksum
	}
	return nil
}

// No DDL is performed here. The last legacy revision owns journal creation,
// including its interrupted-DDL recovery and exact-artifact repair.
func (b *Backend) stampCheckpointOn(ctx context.Context, conn *sql.Conn, chain []revisionArtifact) error {
	root, err := b.verifiedSnapshotOn(ctx, conn, chain)
	if err != nil {
		return err
	}
	if err = validateRevisionSnapshot(ctx, conn, root); err != nil {
		return err
	}
	current, err := currentSnapshot(chain)
	if err != nil {
		return err
	}
	a, err := schemaartifact.Parse(current.sql, "mysql")
	if err != nil || a.Kind != "schema" || a.Epoch != 1 {
		return ErrChecksum
	}
	// The legacy validator allows unknown completed rows for old builds. A
	// designated checkpoint requires the exact known, verified chain instead.
	var unknown int
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM backend_schema_revisions WHERE version<2 OR version>? OR state<>'verified'", chain[len(chain)-1].Version).Scan(&unknown); err != nil {
		return safeError(err)
	}
	if unknown != 0 {
		return ErrChecksum
	}
	sum := fmt.Sprintf("%x", a.Checksum)
	var count int
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_revisions").Scan(&count); err != nil {
		return safeError(err)
	}
	if count == 0 {
		_, err = conn.ExecContext(ctx, `INSERT INTO schema_revisions(epoch,revision,checksum,state,step,verified_at) VALUES(1,0,?,'verified',?,CURRENT_TIMESTAMP(6))`, sum, len(a.Baseline.Steps))
		return safeError(err)
	}
	return validateCheckpointJournal(ctx, conn, sum, len(a.Baseline.Steps))
}

func validateCheckpointJournal(ctx context.Context, conn *sql.Conn, sum string, steps int) error {
	var valid bool
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*)=1 AND COALESCE(MIN(epoch=1 AND revision=0 AND checksum=? AND state='verified' AND step=? AND verified_at>=started_at),FALSE) FROM schema_revisions`, sum, steps).Scan(&valid)
	if err != nil {
		return safeError(err)
	}
	if !valid {
		return ErrChecksum
	}
	return nil
}

func validateBridgeJournal(ctx context.Context, conn *sql.Conn, chain []revisionArtifact) error {
	current, err := currentSnapshot(chain)
	if err != nil {
		return err
	}
	a, err := schemaartifact.Parse(current.sql, "mysql")
	if err != nil {
		return ErrChecksum
	}
	return validateCheckpointJournal(ctx, conn, fmt.Sprintf("%x", a.Checksum), len(a.Baseline.Steps))
}
