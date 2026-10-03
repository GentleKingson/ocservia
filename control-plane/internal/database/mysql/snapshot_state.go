package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const snapshotCommentPrefix = "ocservia-snapshot:"

var snapshotCommentSuffix = regexp.MustCompile(` COMMENT='ocservia-snapshot:[0-9a-f]{64}'$`)

func SnapshotChecksum(engine Engine) (string, error) {
	if engine != MySQL {
		return "", ErrChecksum
	}
	chain, err := loadRevisionChain(engine)
	if err != nil {
		return "", err
	}
	a, err := currentSnapshot(chain)
	return a.sum, err
}

func snapshotMetadataHash(chain []revisionArtifact, name string) string {
	if len(chain) < 29 {
		return ""
	}
	for _, plan := range chain[28].Parents {
		for _, s := range plan.Steps {
			if s.Name == name {
				return s.After
			}
		}
	}
	return ""
}

// The first CREATE atomically binds the otherwise empty metadata table to one
// exact initialization artifact. Only that named table's provenance comment is
// normalized for static shape comparisons; its value is checked independently.
func snapshotStateOn(ctx context.Context, conn *sql.Conn, chain []revisionArtifact) (*snapshotArtifact, string, []string, error) {
	actual, err := schemaHash(ctx, conn, step{Name: "backend_schema_snapshot", Kind: "table"})
	if err != nil {
		return nil, "", nil, err
	}
	if actual == "" {
		return nil, "", nil, nil
	}
	if actual != snapshotMetadataHash(chain, "backend_schema_snapshot") {
		return nil, "", nil, ErrSchema
	}
	var comment string
	if err = conn.QueryRowContext(ctx, "SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='backend_schema_snapshot'").Scan(&comment); err != nil {
		return nil, "", nil, safeError(err)
	}
	var sum, state string
	err = conn.QueryRowContext(ctx, "SELECT artifact_checksum,state FROM backend_schema_snapshot WHERE singleton=1").Scan(&sum, &state)
	if errors.Is(err, sql.ErrNoRows) {
		if comment == "" {
			// An appended legacy revision creates empty provenance tables, not an
			// initialization receipt. No journal rows may exist without an origin.
			hash, e := schemaHash(ctx, conn, step{Name: "backend_schema_snapshot_steps", Kind: "table"})
			if e != nil {
				return nil, "", nil, e
			}
			if hash != "" {
				if hash != snapshotMetadataHash(chain, "backend_schema_snapshot_steps") {
					return nil, "", nil, ErrSchema
				}
				var n int
				if e = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM backend_schema_snapshot_steps").Scan(&n); e != nil {
					return nil, "", nil, safeError(e)
				}
				if n != 0 {
					return nil, "", nil, ErrChecksum
				}
			}
			return nil, "", nil, nil
		}
		if !strings.HasPrefix(comment, snapshotCommentPrefix) {
			return nil, "", nil, ErrChecksum
		}
		a, e := snapshotByChecksum(strings.TrimPrefix(comment, snapshotCommentPrefix), chain)
		if e != nil {
			return nil, "", nil, e
		}
		return &a, "inception", nil, nil
	}
	if err != nil {
		return nil, "", nil, safeError(err)
	}
	if comment != snapshotCommentPrefix+sum {
		return nil, "", nil, ErrChecksum
	}
	a, err := snapshotByChecksum(sum, chain)
	if err != nil {
		return nil, "", nil, err
	}
	if state != "running" && state != "verified" {
		return nil, "", nil, ErrChecksum
	}
	hash, err := schemaHash(ctx, conn, step{Name: "backend_schema_snapshot_steps", Kind: "table"})
	if err != nil {
		return nil, "", nil, err
	}
	if hash == "" {
		if state == "verified" {
			return nil, "", nil, ErrChecksum
		}
		return &a, state, nil, nil
	}
	if hash != snapshotMetadataHash(chain, "backend_schema_snapshot_steps") {
		return nil, "", nil, ErrSchema
	}
	rows, err := conn.QueryContext(ctx, "SELECT ordinal,name,checksum,state FROM backend_schema_snapshot_steps ORDER BY ordinal")
	if err != nil {
		return nil, "", nil, safeError(err)
	}
	defer rows.Close()
	states := []string{}
	for rows.Next() {
		var ordinal int
		var name, checksum, s string
		if err = rows.Scan(&ordinal, &name, &checksum, &s); err != nil {
			return nil, "", nil, safeError(err)
		}
		i := len(states)
		if ordinal != i+1 || i >= len(a.Statements) || name != a.Statements[i].Kind+":"+a.Statements[i].Name || checksum != a.Statements[i].Checksum || (s != "running" && s != "verified") || (i > 0 && states[i-1] != "verified") {
			return nil, "", nil, ErrChecksum
		}
		states = append(states, s)
	}
	if err = rows.Err(); err != nil {
		return nil, "", nil, safeError(err)
	}
	if state == "verified" && (len(states) != len(a.Statements) || states[len(states)-1] != "verified") {
		return nil, "", nil, ErrChecksum
	}
	return &a, state, states, nil
}

func verifiedOriginOn(ctx context.Context, conn *sql.Conn, chain []revisionArtifact) (*snapshotArtifact, error) {
	a, state, _, err := snapshotStateOn(ctx, conn, chain)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, nil
	}
	if state != "verified" {
		return nil, ErrDirty
	}
	root, _, err := baselineFor(MySQL, a.Parent)
	if err != nil {
		return nil, err
	}
	for _, table := range []string{"backend_migrations", "backend_migration_steps"} {
		actual, e := schemaHash(ctx, conn, step{Name: table, Kind: "table"})
		if e != nil {
			return nil, e
		}
		if actual != root.MetadataHashes[table] {
			return nil, ErrSchema
		}
		var n int
		if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return nil, safeError(err)
		}
		if n != 0 {
			return nil, ErrChecksum
		}
	}
	return a, nil
}

func databaseObjectCount(ctx context.Context, conn *sql.Conn) (int, error) {
	var n int
	err := conn.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE())+(SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=DATABASE())+(SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE())+(SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA=DATABASE())`).Scan(&n)
	return n, safeError(err)
}

func snapshotPostcondition(ctx context.Context, conn *sql.Conn, s schemaStatement) (string, error) {
	if s.Kind != "seed" {
		return schemaHash(ctx, conn, step{Name: s.Name, Kind: s.Kind})
	}
	_, values, err := snapshotRows(ctx, conn, s.Name, s.Columns)
	if err != nil {
		return "", safeError(err)
	}
	if len(values) == 0 {
		return "", nil
	}
	if s.Name == "scheduler_leadership" {
		var valid bool
		if err = conn.QueryRowContext(ctx, "SELECT COUNT(*)=1 FROM scheduler_leadership JOIN backend_schema_snapshot ON singleton=1 WHERE updated_at BETWEEN TIMESTAMPDIFF(MICROSECOND,'2000-01-01',started_at) AND TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))").Scan(&valid); err != nil {
			return "", safeError(err)
		}
		if !valid {
			return "", ErrSchema
		}
	}
	data, _ := json.Marshal(values)
	return digest(data), nil
}

func validateSnapshotObjects(ctx context.Context, conn *sql.Conn, a snapshotArtifact, completed int) error {
	// Initialization may contain exactly the journaled static objects; reject
	// rogue views, routines, events, triggers and unjournaled tables on repair.
	expected := 0
	for i, s := range a.Statements {
		if i >= completed {
			break
		}
		if s.Kind != "seed" {
			expected++
		}
	}
	actual, err := databaseObjectCount(ctx, conn)
	if err != nil {
		return err
	}
	if actual != expected {
		return ErrSchema
	}
	return nil
}

func initializeSnapshot(ctx context.Context, conn *sql.Conn, a snapshotArtifact, existing *snapshotArtifact, state string, states []string, repair string) error {
	if existing != nil {
		if existing.sum != a.sum || repair != a.sum {
			return ErrChecksum
		}
		if state == "inception" {
			if err := validateSnapshotObjects(ctx, conn, a, 1); err != nil {
				return err
			}
		}
	} else {
		if repair != "" && repair != a.sum {
			return ErrChecksum
		}
		n, err := databaseObjectCount(ctx, conn)
		if err != nil {
			return err
		}
		if n != 0 {
			return ErrSchema
		}
		if _, err = conn.ExecContext(ctx, snapshotSQL(a, a.Statements[0])+" COMMENT='"+snapshotCommentPrefix+a.sum+"'"); err != nil {
			return safeError(err)
		}
	}
	if existing == nil || state == "inception" {
		if _, err := conn.ExecContext(ctx, "INSERT INTO backend_schema_snapshot(singleton,artifact_checksum,state) VALUES(1,?,'running')", a.sum); err != nil {
			return safeError(err)
		}
	}
	if repair != "" {
		if _, err := conn.ExecContext(ctx, "UPDATE backend_schema_snapshot SET repair_count=repair_count+1 WHERE singleton=1"); err != nil {
			return safeError(err)
		}
	}
	// Genesis is recorded by the atomic comment, then the running origin row.
	// The journal cannot precede its own CREATE; only these two proven metadata
	// DDL executions receive verified receipts after shape checks.
	for i := 0; i < 2; i++ {
		s := a.Statements[i]
		actual, err := snapshotPostcondition(ctx, conn, s)
		if err != nil {
			return err
		}
		if actual == "" && i == 1 {
			if err = validateSnapshotObjects(ctx, conn, a, 1); err != nil {
				return err
			}
			if _, err = conn.ExecContext(ctx, snapshotSQL(a, s)); err != nil {
				return safeError(err)
			}
			actual, err = snapshotPostcondition(ctx, conn, s)
			if err != nil {
				return err
			}
		}
		if actual != s.SchemaHash {
			return ErrSchema
		}
	}
	for i := 0; i < 2; i++ {
		if i < len(states) {
			if states[i] != "verified" {
				return ErrChecksum
			}
			continue
		}
		s := a.Statements[i]
		if _, err := conn.ExecContext(ctx, "INSERT INTO backend_schema_snapshot_steps(ordinal,name,checksum,state,verified_at) VALUES(?,?,?,'verified',CURRENT_TIMESTAMP(6))", i+1, s.Kind+":"+s.Name, s.Checksum); err != nil {
			return safeError(err)
		}
	}
	for i, s := range a.Statements {
		if i < 2 {
			continue
		}
		actual, err := snapshotPostcondition(ctx, conn, s)
		if err != nil {
			return err
		}
		if i < len(states) && states[i] == "verified" {
			if actual != s.SchemaHash {
				return fmt.Errorf("%w: %s", ErrSchema, s.Name)
			}
			continue
		}
		journaled := i < len(states)
		if !journaled {
			if actual != "" {
				return fmt.Errorf("%w: unjournaled snapshot object %s", ErrSchema, s.Name)
			}
			if _, err = conn.ExecContext(ctx, "INSERT INTO backend_schema_snapshot_steps(ordinal,name,checksum,state) VALUES(?,?,?,'running')", i+1, s.Kind+":"+s.Name, s.Checksum); err != nil {
				return safeError(err)
			}
		}
		if actual != "" && actual != s.SchemaHash {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
		if actual == "" {
			if err = validateSnapshotObjects(ctx, conn, a, i); err != nil {
				return err
			}
			if s.Kind == "seed" {
				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					return safeError(err)
				}
				if _, err = tx.ExecContext(ctx, snapshotSQL(a, s)); err != nil {
					tx.Rollback()
					return safeError(err)
				}
				if err = tx.Commit(); err != nil {
					return safeError(err)
				}
			} else if _, err = conn.ExecContext(ctx, snapshotSQL(a, s)); err != nil {
				return fmt.Errorf("snapshot step %s: %w", s.Name, safeError(err))
			}
			actual, err = snapshotPostcondition(ctx, conn, s)
			if err != nil {
				return err
			}
		}
		if actual != s.SchemaHash {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
		if _, err = conn.ExecContext(ctx, "UPDATE backend_schema_snapshot_steps SET state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE ordinal=?", i+1); err != nil {
			return safeError(err)
		}
	}
	if err := validateSnapshotObjects(ctx, conn, a, len(a.Statements)); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "UPDATE backend_schema_snapshot SET state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE singleton=1")
	return safeError(err)
}
