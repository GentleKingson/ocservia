package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// A revision appends to either published version-1 lineage. Baseline receipts
// remain byte-for-byte intact; they never become receipts for a different SQL
// artifact. The version-2 receipt names its actual parent checksum explicitly.
type revisionStep struct {
	Name     string `json:"name"`
	SQL      string `json:"sql"`
	Checksum string `json:"checksum"`
	Before   string `json:"before"`
	After    string `json:"after"`
}
type revisionPlan struct {
	Steps []revisionStep `json:"steps"`
}
type revision struct {
	Version                 int                     `json:"version"`
	Engine                  Engine                  `json:"engine"`
	ControllerSchema        int                     `json:"controller_schema"`
	MinimumControllerSchema int                     `json:"minimum_controller_schema"`
	Parents                 map[string]revisionPlan `json:"parents"`
	MetadataHashes          map[string]string       `json:"metadata_hashes"`
}

const revisionsDDL = `CREATE TABLE IF NOT EXISTS backend_schema_revisions (
 version INT PRIMARY KEY CHECK(version>=2),
 parent_checksum VARBINARY(64) NOT NULL,
 manifest_checksum VARBINARY(64) NOT NULL,
 state VARBINARY(16) NOT NULL CHECK(state IN ('running','verified')),
 repair_count INT NOT NULL DEFAULT 0,
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 verified_at DATETIME(6) NULL
) ENGINE=InnoDB`

const revisionStepsDDL = `CREATE TABLE IF NOT EXISTS backend_schema_revision_steps (
 version INT NOT NULL, ordinal INT NOT NULL,
 name VARBINARY(64) NOT NULL, checksum VARBINARY(64) NOT NULL,
 state VARBINARY(16) NOT NULL CHECK(state IN ('running','verified')),
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 verified_at DATETIME(6) NULL,
 PRIMARY KEY(version,ordinal), UNIQUE KEY(version,name),
 FOREIGN KEY(version) REFERENCES backend_schema_revisions(version) ON DELETE RESTRICT
) ENGINE=InnoDB`

func baselineFor(engine Engine, checksum string) (manifest, string, error) {
	current, sum, err := loadManifest(engine)
	if err != nil {
		return current, "", err
	}
	if checksum == "" || checksum == sum {
		return current, sum, nil
	}
	data, err := manifests.ReadFile("history/f6cd0e0/" + string(engine) + ".json")
	if err != nil {
		return manifest{}, "", ErrChecksum
	}
	old, oldSum, err := decodeManifest(engine, data)
	if err != nil || checksum != oldSum {
		return manifest{}, "", ErrChecksum
	}
	return old, oldSum, nil
}

func loadRevision(engine Engine) (revision, string, error) {
	var r revision
	data, err := manifests.ReadFile(string(engine) + "/000002.json")
	if err != nil || json.Unmarshal(data, &r) != nil {
		return r, "", ErrChecksum
	}
	if r.Engine != engine || r.Version != 2 || r.ControllerSchema != 34 || r.MinimumControllerSchema != 34 || len(r.Parents) != 2 {
		return r, "", ErrChecksum
	}
	if len(r.MetadataHashes) != 2 || len(r.MetadataHashes["backend_schema_revisions"]) != 64 || len(r.MetadataHashes["backend_schema_revision_steps"]) != 64 {
		return r, "", ErrChecksum
	}
	for parent, p := range r.Parents {
		if len(parent) != 64 {
			return r, "", ErrChecksum
		}
		baseline, _, err := baselineFor(engine, parent)
		if err != nil {
			return r, "", err
		}
		before := map[string]string{}
		for _, s := range baseline.Steps {
			if s.Kind == "table" {
				before[s.Name] = s.SchemaHash
			}
		}
		seen := map[string]bool{}
		for _, s := range p.Steps {
			if seen[s.Name] || !identifier.MatchString(s.Name) || before[s.Name] != s.Before || len(s.After) != 64 || s.Before == s.After || digest([]byte(s.SQL)) != s.Checksum {
				return r, "", ErrChecksum
			}
			seen[s.Name] = true
		}
	}
	return r, digest(data), nil
}

func (b *Backend) rootOn(ctx context.Context, conn *sql.Conn) (manifest, string, error) {
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='backend_migrations'`).Scan(&exists); err != nil {
		return manifest{}, "", safeError(err)
	}
	var sum string
	if exists != 0 {
		err := conn.QueryRowContext(ctx, `SELECT manifest_checksum FROM backend_migrations WHERE singleton=1`).Scan(&sum)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return manifest{}, "", safeError(err)
		}
	}
	return baselineFor(b.engine, sum)
}

// revisionTables validates any already-created metadata before using it. Two
// independent DDL commits may be interrupted; neither implies a business step
// ran or a revision completed.
func revisionTables(ctx context.Context, conn *sql.Conn, r revision) (int, error) {
	n := 0
	for _, name := range []string{"backend_schema_revisions", "backend_schema_revision_steps"} {
		actual, err := schemaHash(ctx, conn, step{Name: name, Kind: "table"})
		if err != nil {
			return 0, err
		}
		if actual == "" {
			continue
		}
		if len(r.MetadataHashes[name]) != 64 || actual != r.MetadataHashes[name] {
			return 0, ErrSchema
		}
		n++
	}
	return n, nil
}

func revisionState(ctx context.Context, conn *sql.Conn, r revision, parent, sum string) (string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version,parent_checksum,manifest_checksum,state FROM backend_schema_revisions ORDER BY version`)
	if err != nil {
		return "", safeError(err)
	}
	defer rows.Close()
	state := ""
	for rows.Next() {
		var v int
		var p, s, st string
		if err = rows.Scan(&v, &p, &s, &st); err != nil {
			return "", safeError(err)
		}
		if state != "" || v != r.Version || p != parent || s != sum || (st != "running" && st != "verified") {
			return "", ErrChecksum
		}
		state = st
	}
	return state, safeError(rows.Err())
}

func revisionStates(ctx context.Context, conn *sql.Conn, r revision, p revisionPlan) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version,ordinal,name,checksum,state FROM backend_schema_revision_steps ORDER BY version,ordinal`)
	if err != nil {
		return nil, safeError(err)
	}
	defer rows.Close()
	states := []string{}
	for rows.Next() {
		var v, ordinal int
		var name, sum, state string
		if err = rows.Scan(&v, &ordinal, &name, &sum, &state); err != nil {
			return nil, safeError(err)
		}
		i := len(states)
		if v != r.Version || ordinal != i+1 || i >= len(p.Steps) || name != p.Steps[i].Name || sum != p.Steps[i].Checksum || (state != "running" && state != "verified") || (i > 0 && states[i-1] != "verified") {
			return nil, ErrChecksum
		}
		states = append(states, state)
	}
	return states, safeError(rows.Err())
}

func revisedSnapshot(root manifest, p revisionPlan) manifest {
	result := root
	result.Steps = append([]step(nil), root.Steps...)
	for _, change := range p.Steps {
		for i := range result.Steps {
			if result.Steps[i].Kind == "table" && result.Steps[i].Name == change.Name {
				result.Steps[i].SchemaHash = change.After
			}
		}
	}
	return result
}

func (b *Backend) Migrate(ctx context.Context, repairChecksum string) (result error) {
	r, sum, err := loadRevision(b.engine)
	if err != nil {
		return err
	}
	if repairChecksum != "" && repairChecksum != sum {
		return ErrChecksum
	}
	conn, name, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, name)) }()
	root, parent, err := b.rootOn(ctx, conn)
	if err != nil {
		return err
	}
	plan, ok := r.Parents[parent]
	if !ok {
		return ErrChecksum
	}
	tables, err := revisionTables(ctx, conn, r)
	if err != nil {
		return err
	}
	state := ""
	if tables == 2 {
		state, err = revisionState(ctx, conn, r, parent, sum)
		if err != nil {
			return err
		}
	}
	if state == "" {
		if tables == 0 {
			token := ""
			if repairChecksum != "" {
				token = parent
			}
			if err = b.migrateBaseline(ctx, conn, root, parent, token); err != nil {
				return err
			}
		}
		if err = b.validateOn(ctx, conn, root, parent, tables); err != nil {
			return err
		}
		if err = validateSnapshot(ctx, conn, root); err != nil {
			return err
		}
		for _, ddl := range []string{revisionsDDL, revisionStepsDDL} {
			if _, err = conn.ExecContext(ctx, ddl); err != nil {
				return safeError(err)
			}
		}
		if n, err := revisionTables(ctx, conn, r); err != nil || n != 2 {
			if err != nil {
				return err
			}
			return ErrSchema
		}
		states, err := revisionStates(ctx, conn, r, plan)
		if err != nil {
			return err
		}
		if len(states) != 0 {
			return ErrChecksum
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO backend_schema_revisions(version,parent_checksum,manifest_checksum,state) VALUES(?,?,?,'running')`, r.Version, parent, sum); err != nil {
			return safeError(err)
		}
	} else {
		if state == "running" && repairChecksum == "" {
			return ErrDirty
		}
		if err = b.validateOn(ctx, conn, root, parent, 2); err != nil {
			return err
		}
	}
	states, err := revisionStates(ctx, conn, r, plan)
	if err != nil {
		return err
	}
	if state == "verified" {
		if len(states) != len(plan.Steps) || (len(states) > 0 && states[len(states)-1] != "verified") {
			return ErrChecksum
		}
		return validateSnapshot(ctx, conn, revisedSnapshot(root, plan))
	}
	if repairChecksum != "" {
		if _, err = conn.ExecContext(ctx, `UPDATE backend_schema_revisions SET repair_count=repair_count+1 WHERE version=?`, r.Version); err != nil {
			return safeError(err)
		}
	}
	for i, s := range plan.Steps {
		object := step{Name: s.Name, Kind: "table"}
		actual, err := schemaHash(ctx, conn, object)
		if err != nil {
			return err
		}
		if i < len(states) && states[i] == "verified" {
			if actual != s.After {
				return fmt.Errorf("%w: %s", ErrSchema, s.Name)
			}
			continue
		}
		if i >= len(states) {
			if actual != s.Before {
				return fmt.Errorf("%w: unjournaled revision object %s", ErrSchema, s.Name)
			}
			if _, err = conn.ExecContext(ctx, `INSERT INTO backend_schema_revision_steps(version,ordinal,name,checksum,state) VALUES(?,?,?,?,'running')`, r.Version, i+1, s.Name, s.Checksum); err != nil {
				return safeError(err)
			}
		}
		if actual == s.Before {
			if _, err = conn.ExecContext(ctx, s.SQL); err != nil {
				return fmt.Errorf("revision %d step %s: %w", r.Version, s.Name, safeError(err))
			}
			actual, err = schemaHash(ctx, conn, object)
			if err != nil {
				return err
			}
		}
		if actual != s.After {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
		if _, err = conn.ExecContext(ctx, `UPDATE backend_schema_revision_steps SET state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE version=? AND ordinal=?`, r.Version, i+1); err != nil {
			return safeError(err)
		}
	}
	// DDL is not transactional. The clean version receipt is published only
	// after every statement receipt and the complete resulting schema agree.
	if err = validateSnapshot(ctx, conn, revisedSnapshot(root, plan)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `UPDATE backend_schema_revisions SET state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE version=?`, r.Version)
	return safeError(err)
}

func (b *Backend) ValidateSchema(ctx context.Context, expectedController int) (result error) {
	r, sum, err := loadRevision(b.engine)
	if err != nil {
		return err
	}
	if expectedController < r.MinimumControllerSchema || expectedController > r.ControllerSchema {
		return ErrSchema
	}
	conn, name, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, name)) }()
	root, parent, err := b.rootOn(ctx, conn)
	if err != nil {
		return err
	}
	plan, ok := r.Parents[parent]
	if !ok {
		return ErrChecksum
	}
	if n, err := revisionTables(ctx, conn, r); err != nil || n != 2 {
		if err != nil {
			return err
		}
		return ErrSchema
	}
	state, err := revisionState(ctx, conn, r, parent, sum)
	if err != nil {
		return err
	}
	if state == "running" {
		return ErrDirty
	}
	if state != "verified" {
		return ErrSchema
	}
	if err = b.validateOn(ctx, conn, root, parent, 2); err != nil {
		return err
	}
	states, err := revisionStates(ctx, conn, r, plan)
	if err != nil {
		return err
	}
	if len(states) != len(plan.Steps) || (len(states) > 0 && states[len(states)-1] != "verified") {
		return ErrChecksum
	}
	return validateSnapshot(ctx, conn, revisedSnapshot(root, plan))
}
