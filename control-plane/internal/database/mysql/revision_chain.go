package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const latestRevisionVersion = 6

type revisionArtifact struct {
	revision
	sum string
}

func revisionObject(s revisionStep) step {
	name, kind := s.Object, s.Kind
	if name == "" {
		name = s.Name
	}
	if kind == "" {
		kind = "table"
	}
	return step{Name: name, Kind: kind, SchemaHash: s.After}
}

func revisedSnapshot(root manifest, p revisionPlan) manifest {
	result := root
	result.Steps = append([]step(nil), root.Steps...)
	for _, change := range p.Steps {
		object := revisionObject(change)
		if object.Kind == "data" {
			continue
		}
		found := false
		for i, existing := range result.Steps {
			if existing.Kind == object.Kind && existing.Name == object.Name {
				found = true
				if change.After == "" {
					result.Steps = append(result.Steps[:i], result.Steps[i+1:]...)
				} else {
					result.Steps[i] = object
				}
				break
			}
		}
		if !found && change.After != "" {
			result.Steps = append(result.Steps, object)
		}
	}
	return result
}

func validateRevisionPlan(snapshot manifest, p revisionPlan) (manifest, error) {
	seen := map[string]bool{}
	for _, s := range p.Steps {
		object := revisionObject(s)
		if !identifier.MatchString(s.Name) || len(s.Name) > 64 || seen[s.Name] || s.SQL == "" || digest([]byte(s.SQL)) != s.Checksum {
			return snapshot, ErrChecksum
		}
		seen[s.Name] = true
		if object.Kind == "data" {
			if s.VerifySQL == "" || s.Before != "" || s.After != digest([]byte("valid")) || s.CheckBeforeSQL != "" {
				return snapshot, ErrChecksum
			}
			continue
		}
		if (object.Kind != "table" && object.Kind != "trigger" && object.Kind != "procedure" && object.Kind != "function") || !identifier.MatchString(object.Name) || len(object.Name) > 64 || s.VerifySQL != "" || s.Repairable {
			return snapshot, ErrChecksum
		}
		if (len(s.Before) != 0 && len(s.Before) != 64) || (len(s.After) != 0 && len(s.After) != 64) || s.Before == s.After {
			return snapshot, ErrChecksum
		}
		before := ""
		for _, prior := range snapshot.Steps {
			if prior.Name == object.Name && prior.Kind == object.Kind {
				before = prior.SchemaHash
			}
		}
		if before != s.Before {
			return snapshot, ErrChecksum
		}
		snapshot = revisedSnapshot(snapshot, revisionPlan{Steps: []revisionStep{s}})
	}
	return snapshot, nil
}

func loadRevisionChain(engine Engine) ([]revisionArtifact, error) {
	r, sum, err := loadRevision(engine)
	if err != nil {
		return nil, err
	}
	chain := []revisionArtifact{{r, sum}}
	snapshots := map[string]manifest{}
	for parent, plan := range r.Parents {
		root, _, err := baselineFor(engine, parent)
		if err != nil {
			return nil, err
		}
		snapshots[parent] = revisedSnapshot(root, plan)
	}
	for version := 3; version <= latestRevisionVersion; version++ {
		data, err := manifests.ReadFile(fmt.Sprintf("%s/%06d.json", engine, version))
		var next revision
		if err != nil || json.Unmarshal(data, &next) != nil {
			return nil, ErrChecksum
		}
		if next.Engine != engine || next.Version != version || next.PreviousChecksum != sum || next.ControllerSchema != 34 || next.MinimumControllerSchema != 34 || len(next.Parents) != len(snapshots) || len(next.MetadataHashes) != 2 {
			return nil, ErrChecksum
		}
		for name, hash := range r.MetadataHashes {
			if next.MetadataHashes[name] != hash {
				return nil, ErrChecksum
			}
		}
		for parent, snapshot := range snapshots {
			plan, ok := next.Parents[parent]
			if !ok {
				return nil, ErrChecksum
			}
			snapshot, err = validateRevisionPlan(snapshot, plan)
			if err != nil {
				return nil, err
			}
			snapshots[parent] = snapshot
		}
		sum = digest(data)
		chain = append(chain, revisionArtifact{next, sum})
	}
	return chain, nil
}

// Read the complete ledger, not just MAX(version). Unknown versions, omitted
// parents, dirty predecessors and orphan/reordered step receipts are refused.
func readRevisionHistory(ctx context.Context, conn *sql.Conn, chain []revisionArtifact, parent string) ([]string, [][]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version,parent_checksum,manifest_checksum,state FROM backend_schema_revisions ORDER BY version`)
	if err != nil {
		return nil, nil, safeError(err)
	}
	states := []string{}
	for rows.Next() {
		var version int
		var previous, sum, state string
		if err = rows.Scan(&version, &previous, &sum, &state); err != nil {
			break
		}
		i := len(states)
		expectedParent := parent
		if i > 0 {
			expectedParent = chain[i-1].sum
		}
		if i >= len(chain) || version != chain[i].Version || previous != expectedParent || sum != chain[i].sum || (state != "running" && state != "verified") || (i > 0 && states[i-1] != "verified") {
			err = ErrChecksum
			break
		}
		states = append(states, state)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if errors.Is(err, ErrChecksum) {
		return nil, nil, ErrChecksum
	}
	if err != nil {
		return nil, nil, safeError(err)
	}
	steps := make([][]string, len(chain))
	rows, err = conn.QueryContext(ctx, `SELECT version,ordinal,name,checksum,state FROM backend_schema_revision_steps ORDER BY version,ordinal`)
	if err != nil {
		return nil, nil, safeError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var version, ordinal int
		var name, sum, state string
		if err = rows.Scan(&version, &ordinal, &name, &sum, &state); err != nil {
			return nil, nil, safeError(err)
		}
		i := version - 2
		if i < 0 || i >= len(states) {
			return nil, nil, ErrChecksum
		}
		plan, ok := chain[i].Parents[parent]
		j := len(steps[i])
		if !ok || ordinal != j+1 || j >= len(plan.Steps) || name != plan.Steps[j].Name || sum != plan.Steps[j].Checksum || (state != "running" && state != "verified") || (j > 0 && steps[i][j-1] != "verified") {
			return nil, nil, ErrChecksum
		}
		steps[i] = append(steps[i], state)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, safeError(err)
	}
	for i, state := range states {
		if state == "verified" && (len(steps[i]) != len(chain[i].Parents[parent].Steps) || (len(steps[i]) > 0 && steps[i][len(steps[i])-1] != "verified")) {
			return nil, nil, ErrChecksum
		}
	}
	return states, steps, nil
}

func revisionPostcondition(ctx context.Context, conn *sql.Conn, s revisionStep) (string, error) {
	if s.Kind != "data" {
		return schemaHash(ctx, conn, revisionObject(s))
	}
	var result string
	if err := conn.QueryRowContext(ctx, s.VerifySQL).Scan(&result); err != nil {
		return "", safeError(err)
	}
	return digest([]byte(result)), nil
}

func checkRevisionBefore(ctx context.Context, conn *sql.Conn, s revisionStep) error {
	if s.CheckBeforeSQL == "" {
		return nil
	}
	var result string
	if err := conn.QueryRowContext(ctx, s.CheckBeforeSQL).Scan(&result); err != nil {
		return safeError(err)
	}
	if result != "valid" {
		return fmt.Errorf("%w: %s source copy changed", ErrSchema, s.Name)
	}
	return nil
}

func applyRevision(ctx context.Context, conn *sql.Conn, artifact revisionArtifact, plan revisionPlan, states []string) error {
	guards := map[string]string{}
	for i, s := range plan.Steps {
		if i >= len(states) {
			break
		}
		object := revisionObject(s)
		if object.Kind != "trigger" || !strings.HasPrefix(object.Name, "migrate_") {
			continue
		}
		expected := s.After
		if states[i] == "running" {
			actual, err := schemaHash(ctx, conn, object)
			if err != nil {
				return err
			}
			if actual != s.Before && actual != s.After {
				return ErrSchema
			}
			expected = actual
		}
		guards[object.Name] = expected
	}
	for name, want := range guards {
		actual, err := schemaHash(ctx, conn, step{Name: name, Kind: "trigger"})
		if err != nil {
			return err
		}
		if actual != want {
			return fmt.Errorf("%w: migration writer guard %s", ErrSchema, name)
		}
	}
	for i, s := range plan.Steps {
		if i < len(states) && states[i] == "verified" {
			// Another step may have subsequently altered the same object. The
			// whole resulting snapshot is verified before publishing the version.
			continue
		}
		actual, err := revisionPostcondition(ctx, conn, s)
		if err != nil {
			return err
		}
		journaled := i < len(states)
		if !journaled {
			if s.Kind != "data" && actual != s.Before {
				return fmt.Errorf("%w: unjournaled revision object %s", ErrSchema, s.Name)
			}
			if _, err = conn.ExecContext(ctx, `INSERT INTO backend_schema_revision_steps(version,ordinal,name,checksum,state) VALUES(?,?,?,?,'running')`, artifact.Version, i+1, s.Name, s.Checksum); err != nil {
				return safeError(err)
			}
		}
		execute := !journaled || actual != s.After
		if execute {
			if journaled && ((s.Kind == "data" && !s.Repairable) || (s.Kind != "data" && actual != s.Before)) {
				return fmt.Errorf("%w: %s", ErrSchema, s.Name)
			}
			if err = checkRevisionBefore(ctx, conn, s); err != nil {
				return err
			}
			if _, err = conn.ExecContext(ctx, s.SQL); err != nil {
				return fmt.Errorf("revision %d step %s: %w", artifact.Version, s.Name, safeError(err))
			}
			actual, err = revisionPostcondition(ctx, conn, s)
			if err != nil {
				return err
			}
		}
		if actual != s.After {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
		if _, err = conn.ExecContext(ctx, `UPDATE backend_schema_revision_steps SET state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE version=? AND ordinal=?`, artifact.Version, i+1); err != nil {
			return safeError(err)
		}
	}
	return nil
}

func validateRevisionSnapshot(ctx context.Context, conn *sql.Conn, snapshot manifest) error {
	if err := validateSnapshot(ctx, conn, snapshot); err != nil {
		return err
	}
	extra := 2
	hasCatalog, hasTemplate := false, false
	for _, s := range snapshot.Steps {
		if s.Kind == "table" && s.Name == "telemetry_sample_shards" {
			hasCatalog = true
		}
		if s.Kind == "table" && s.Name == "telemetry_samples_template" {
			hasTemplate = true
		}
	}
	if hasCatalog != hasTemplate {
		return ErrSchema
	}
	if hasCatalog {
		names, err := validateTelemetryShards(ctx, conn)
		if err != nil {
			return err
		}
		extra += len(names)
	}
	return validateObjectCounts(ctx, conn, snapshot, extra)
}

func (b *Backend) Migrate(ctx context.Context, repairChecksum string) (result error) {
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		return err
	}
	if repairChecksum != "" && repairChecksum != chain[len(chain)-1].sum {
		return ErrChecksum
	}
	conn, name, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, name)) }()
	return b.migrateChainOn(ctx, conn, chain, repairChecksum)
}

func (b *Backend) migrateChainOn(ctx context.Context, conn *sql.Conn, chain []revisionArtifact, repairChecksum string) error {
	root, parent, err := b.rootOn(ctx, conn)
	if err != nil {
		return err
	}
	for _, r := range chain {
		if _, ok := r.Parents[parent]; !ok {
			return ErrChecksum
		}
	}
	tables, err := revisionTables(ctx, conn, chain[0].revision)
	if err != nil {
		return err
	}
	var states []string
	var steps [][]string
	if tables == 2 {
		states, steps, err = readRevisionHistory(ctx, conn, chain, parent)
		if err != nil {
			return err
		}
	}
	if len(states) == 0 {
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
		if n, err := revisionTables(ctx, conn, chain[0].revision); err != nil || n != 2 {
			if err != nil {
				return err
			}
			return ErrSchema
		}
		states, steps, err = readRevisionHistory(ctx, conn, chain, parent)
		if err != nil {
			return err
		}
	}
	if err = b.validateBaselineReceipts(ctx, conn, root, parent); err != nil {
		return err
	}
	for _, state := range states {
		if state == "running" && repairChecksum == "" {
			return ErrDirty
		}
	}
	snapshot := root
	for i, r := range chain {
		plan := r.Parents[parent]
		target := revisedSnapshot(snapshot, plan)
		if i < len(states) && states[i] == "verified" {
			snapshot = target
			continue
		}
		if i >= len(states) {
			if err = validateRevisionSnapshot(ctx, conn, snapshot); err != nil {
				return err
			}
			previous := parent
			if i > 0 {
				previous = chain[i-1].sum
			}
			if _, err = conn.ExecContext(ctx, `INSERT INTO backend_schema_revisions(version,parent_checksum,manifest_checksum,state) VALUES(?,?,?,'running')`, r.Version, previous, r.sum); err != nil {
				return safeError(err)
			}
		} else if repairChecksum != "" {
			if _, err = conn.ExecContext(ctx, `UPDATE backend_schema_revisions SET repair_count=repair_count+1 WHERE version=?`, r.Version); err != nil {
				return safeError(err)
			}
		}
		if err = applyRevision(ctx, conn, r, plan, steps[i]); err != nil {
			return err
		}
		if err = validateRevisionSnapshot(ctx, conn, target); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `UPDATE backend_schema_revisions SET state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE version=?`, r.Version); err != nil {
			return safeError(err)
		}
		snapshot = target
	}
	return validateRevisionSnapshot(ctx, conn, snapshot)
}

func (b *Backend) ValidateSchema(ctx context.Context, expectedController int) (result error) {
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		return err
	}
	latest := chain[len(chain)-1]
	if expectedController < latest.MinimumControllerSchema || expectedController > latest.ControllerSchema {
		return ErrSchema
	}
	conn, name, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, name)) }()
	snapshot, err := b.verifiedSnapshotOn(ctx, conn, chain)
	if err != nil {
		return err
	}
	return validateRevisionSnapshot(ctx, conn, snapshot)
}

// verifiedSnapshotOn checks immutable history and every static object without
// requiring an interrupted owner-managed dynamic shard to be active already.
func (b *Backend) verifiedSnapshotOn(ctx context.Context, conn *sql.Conn, chain []revisionArtifact) (manifest, error) {
	root, parent, err := b.rootOn(ctx, conn)
	if err != nil {
		return manifest{}, err
	}
	if n, err := revisionTables(ctx, conn, chain[0].revision); err != nil || n != 2 {
		if err != nil {
			return manifest{}, err
		}
		return manifest{}, ErrSchema
	}
	states, _, err := readRevisionHistory(ctx, conn, chain, parent)
	if err != nil {
		return manifest{}, err
	}
	for _, state := range states {
		if state == "running" {
			return manifest{}, ErrDirty
		}
	}
	if len(states) != len(chain) {
		return manifest{}, ErrSchema
	}
	if err = b.validateBaselineReceipts(ctx, conn, root, parent); err != nil {
		return manifest{}, err
	}
	snapshot := root
	for _, r := range chain {
		plan, ok := r.Parents[parent]
		if !ok {
			return manifest{}, ErrChecksum
		}
		snapshot = revisedSnapshot(snapshot, plan)
	}
	return snapshot, validateSnapshot(ctx, conn, snapshot)
}
