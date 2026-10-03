package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
)

const artifactCommentPrefix = "ocservia-schema:"

var artifactCommentSuffix = regexp.MustCompile(` COMMENT='ocservia-schema:[0-9a-f]{64}'$`)

type mysqlArtifacts struct {
	schema, upgrade schemaartifact.Artifact
	metadata        map[string]artifactStepMetadata
	shapes          []manifest
	known           map[string]bool
	roundtrip       map[string]string
	journal         schemaartifact.Step
}

func stepKey(s schemaartifact.Step) string    { return fmt.Sprintf("%x:%s", s.Checksum, s.Metadata) }
func objectKey(m artifactStepMetadata) string { return m.Kind + ":" + m.Object }
func validHash(s string) bool                 { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
func decodeArtifactMetadata(s schemaartifact.Step) (artifactStepMetadata, error) {
	var m artifactStepMetadata
	if json.Unmarshal(s.Metadata, &m) != nil {
		return m, ErrChecksum
	}
	canonical, _ := json.Marshal(m)
	if !bytes.Equal(canonical, s.Metadata) || !identifier.MatchString(m.Object) || !strings.HasSuffix(string(s.SQL), ";\n") {
		return m, ErrChecksum
	}
	for _, h := range []string{m.Before, m.After, m.RoundtripHash} {
		if h != "" && !validHash(h) {
			return m, ErrChecksum
		}
	}
	for _, c := range m.Columns {
		if !identifier.MatchString(c) {
			return m, ErrChecksum
		}
	}
	switch m.Kind {
	case "table", "trigger", "procedure", "function":
		if m.Before == m.After || m.VerifySQL != "" || m.CheckBeforeSQL != "" || m.Repairable || len(m.Columns) != 0 {
			return m, ErrChecksum
		}
	case "seed":
		if m.Before != "" || !validHash(m.After) || len(m.Columns) == 0 || m.VerifySQL != "" || m.CheckBeforeSQL != "" || m.Repairable || m.RoundtripHash != "" {
			return m, ErrChecksum
		}
	case "data":
		if m.Before != "" || m.After != digest([]byte("valid")) || m.VerifySQL == "" || len(m.Columns) != 0 || m.RoundtripHash != "" {
			return m, ErrChecksum
		}
	default:
		return m, ErrChecksum
	}
	return m, nil
}
func parseMySQLArtifacts(schema, upgrade []byte) (mysqlArtifacts, error) {
	a := mysqlArtifacts{metadata: map[string]artifactStepMetadata{}, known: map[string]bool{}, roundtrip: map[string]string{}}
	var err error
	a.schema, err = schemaartifact.Parse(schema, "mysql")
	if err != nil {
		return a, fmt.Errorf("%w: schema markers: %v", ErrChecksum, err)
	}
	a.upgrade, err = schemaartifact.Parse(upgrade, "mysql")
	if err != nil {
		return a, fmt.Errorf("%w: upgrade markers: %v", ErrChecksum, err)
	}
	head := int64(len(a.upgrade.Revisions))
	if a.schema.Kind != "schema" || a.upgrade.Kind != "upgrade" || a.schema.Epoch != a.upgrade.Epoch || a.schema.Baseline.Number != head || a.upgrade.Base == nil || (a.schema.Epoch > 1 && a.upgrade.Previous == nil) {
		return a, fmt.Errorf("%w: artifact window", ErrChecksum)
	}
	cp := a.upgrade.Base
	if head > 0 {
		cp = a.upgrade.Revisions[head-1].Checkpoint
		history, err := schemaartifact.HistoryChecksum(a.upgrade, head)
		if err != nil || a.schema.HistoryChecksum != history {
			return a, fmt.Errorf("%w: covered history", ErrChecksum)
		}
	}
	if cp == nil || cp.Checksum != fmt.Sprintf("%x", a.schema.Checksum) || cp.Steps != len(a.schema.Baseline.Steps) {
		return a, fmt.Errorf("%w: schema checkpoint", ErrChecksum)
	}
	all := append([]schemaartifact.Step(nil), a.schema.Baseline.Steps...)
	for _, r := range a.upgrade.Revisions {
		all = append(all, r.Steps...)
	}
	if a.upgrade.Transition != nil {
		all = append(all, a.upgrade.Transition.Steps...)
	}
	for _, s := range all {
		m, err := decodeArtifactMetadata(s)
		if err != nil {
			return a, fmt.Errorf("%w: metadata for %s", err, s.Name)
		}
		a.metadata[stepKey(s)] = m
		if m.Before != "" {
			a.known[m.Before] = true
		}
		if m.After != "" {
			a.known[m.After] = true
		}
		if m.RoundtripHash != "" {
			a.roundtrip[m.RoundtripHash] = m.After
		}
	}
	final := map[string]step{}
	for _, s := range a.schema.Baseline.Steps {
		m := a.meta(s)
		if m.Kind == "data" || m.Before != "" {
			return a, fmt.Errorf("%w: schema before state %s", ErrChecksum, s.Name)
		}
		if m.Kind == "seed" {
			continue
		}
		if m.After == "" || final[objectKey(m)].Name != "" {
			return a, fmt.Errorf("%w: duplicate/schema object %s", ErrChecksum, s.Name)
		}
		final[objectKey(m)] = step{Name: m.Object, Kind: m.Kind, SchemaHash: m.After}
		if m.Object == "schema_revisions" && m.Kind == "table" {
			a.journal = s
		}
	}
	if len(a.journal.SQL) == 0 {
		return a, ErrChecksum
	}
	// Reverse explicit before/after metadata to recover the epoch baseline.
	// SQL remains opaque and is never split or parsed.
	shapes := make([]map[string]step, head+1)
	shapes[head] = final
	for i := int(head) - 1; i >= 0; i-- {
		before := cloneShape(shapes[i+1])
		r := a.upgrade.Revisions[i]
		for j := len(r.Steps) - 1; j >= 0; j-- {
			m := a.meta(r.Steps[j])
			if m.Kind == "data" {
				continue
			}
			if m.Kind == "seed" {
				return a, ErrChecksum
			}
			if before[objectKey(m)].SchemaHash != m.After {
				return a, fmt.Errorf("%w: revision after state %s", ErrChecksum, r.Steps[j].Name)
			}
			if m.Before == "" {
				delete(before, objectKey(m))
			} else {
				before[objectKey(m)] = step{Name: m.Object, Kind: m.Kind, SchemaHash: m.Before}
			}
		}
		shapes[i] = before
	}
	for _, shape := range shapes {
		a.shapes = append(a.shapes, a.manifest(shape))
	}
	if p := a.upgrade.Previous; p != nil {
		if p.Receipt == nil {
			return a, ErrChecksum
		}
		var prior map[string]step
		if json.Unmarshal(p.Receipt.Metadata, &prior) != nil || len(prior) == 0 {
			return a, ErrChecksum
		}
		for k, s := range prior {
			a.known[s.SchemaHash] = true
			if k != s.Kind+":"+s.Name || !identifier.MatchString(s.Name) || !validHash(s.SchemaHash) {
				return a, ErrChecksum
			}
		}
		target := cloneShape(prior)
		for _, s := range a.upgrade.Transition.Steps {
			m := a.meta(s)
			if m.Kind == "data" {
				continue
			}
			if m.Kind == "seed" || target[objectKey(m)].SchemaHash != m.Before {
				return a, ErrChecksum
			}
			applyShape(target, m)
		}
		if !sameShape(target, shapes[0]) {
			return a, ErrChecksum
		}
	}
	return a, nil
}
func (a mysqlArtifacts) meta(s schemaartifact.Step) artifactStepMetadata {
	return a.metadata[stepKey(s)]
}
func cloneShape(in map[string]step) map[string]step {
	out := map[string]step{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func sameShape(a, b map[string]step) bool {
	if len(a) != len(b) {
		return false
	}
	for k, s := range a {
		if b[k].SchemaHash != s.SchemaHash {
			return false
		}
	}
	return true
}
func applyShape(shape map[string]step, m artifactStepMetadata) {
	if m.Kind == "data" || m.Kind == "seed" {
		return
	}
	if m.After == "" {
		delete(shape, objectKey(m))
	} else {
		shape[objectKey(m)] = step{Name: m.Object, Kind: m.Kind, SchemaHash: m.After}
	}
}
func (a mysqlArtifacts) manifest(shape map[string]step) manifest {
	m := manifest{artifactOwned: true, fingerprint: a.fingerprint}
	for _, s := range shape {
		m.Steps = append(m.Steps, s)
	}
	return m
}
func shapeOf(m manifest) map[string]step {
	out := map[string]step{}
	for _, s := range m.Steps {
		out[s.Kind+":"+s.Name] = s
	}
	return out
}
func (a mysqlArtifacts) fingerprint(definition string) string {
	exact := digest([]byte(definition))
	if a.known[exact] {
		return exact
	}
	if original := a.roundtrip[exact]; original != "" && a.known[original] {
		return original
	}
	if strings.HasSuffix(definition, "DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin") {
		candidate := digest([]byte(redundantColumnCharset.ReplaceAllString(definition, "${1} COLLATE utf8mb4_0900_bin")))
		if a.known[candidate] {
			return candidate
		}
		if original := a.roundtrip[candidate]; original != "" && a.known[original] {
			return original
		}
	}
	return exact
}
func loadMySQLArtifacts() (mysqlArtifacts, error) {
	schema, err := manifests.ReadFile("mysql/schema.sql")
	if err != nil {
		return mysqlArtifacts{}, ErrChecksum
	}
	upgrade, err := manifests.ReadFile("mysql/upgrade.sql")
	if err != nil {
		return mysqlArtifacts{}, ErrChecksum
	}
	return parseMySQLArtifacts(schema, upgrade)
}

type mysqlReceipt struct {
	epoch, revision int64
	checksum, state string
	step            int
	started         time.Time
	verified        sql.NullTime
}

func readMySQLReceipts(ctx context.Context, conn *sql.Conn) ([]mysqlReceipt, error) {
	rows, err := conn.QueryContext(ctx, "SELECT epoch,revision,checksum,state,step,started_at,verified_at FROM schema_revisions ORDER BY epoch,revision")
	if err != nil {
		return nil, safeError(err)
	}
	defer rows.Close()
	var receipts []mysqlReceipt
	for rows.Next() {
		var r mysqlReceipt
		if err = rows.Scan(&r.epoch, &r.revision, &r.checksum, &r.state, &r.step, &r.started, &r.verified); err != nil {
			return nil, safeError(err)
		}
		receipts = append(receipts, r)
	}
	return receipts, safeError(rows.Err())
}
func verifiedReceipt(r mysqlReceipt, cp *schemaartifact.Receipt) bool {
	return cp != nil && r.checksum == cp.Checksum && r.step == cp.Steps && r.state == "verified" && r.verified.Valid && !r.verified.Time.Before(r.started)
}

// Admission is read-only, including exact repair authorization, before DDL or journal writes.
func (a mysqlArtifacts) admit(rows []mysqlReceipt, repair string) (int64, *mysqlReceipt, bool, error) {
	if len(rows) == 0 {
		return 0, nil, false, ErrChecksum
	}
	current := rows
	previous := false
	if rows[0].epoch != a.schema.Epoch {
		p := a.upgrade.Previous
		if p == nil || rows[0].epoch != p.Epoch || rows[0].revision != p.Revision || !verifiedReceipt(rows[0], p.Receipt) {
			return 0, nil, false, ErrChecksum
		}
		previous = true
		current = rows[1:]
		if len(current) == 0 {
			return 0, nil, true, nil
		}
	}
	var last int64
	for i, r := range current {
		if r.epoch != a.schema.Epoch || r.revision < 0 || r.revision > int64(len(a.upgrade.Revisions)) || (i > 0 && r.revision != last+1) {
			return 0, nil, false, ErrChecksum
		}
		cp := a.upgrade.Base
		if i == 0 && r.revision > 0 {
			cp = a.upgrade.Revisions[r.revision-1].Checkpoint
		}
		sum := ""
		steps := 0
		bootstrap := i == 0 && !previous
		if bootstrap {
			if cp == nil {
				return 0, nil, false, ErrChecksum
			}
			sum, steps = cp.Checksum, cp.Steps
		} else if i == 0 {
			if r.revision != 0 {
				return 0, nil, false, ErrChecksum
			}
			sum, steps = fmt.Sprintf("%x", a.upgrade.Transition.Checksum), len(a.upgrade.Transition.Steps)
		} else {
			rev := a.upgrade.Revisions[r.revision-1]
			sum, steps = fmt.Sprintf("%x", rev.Checksum), len(rev.Steps)
		}
		if r.state == "verified" {
			if !verifiedReceipt(r, &schemaartifact.Receipt{Checksum: sum, Steps: steps}) {
				return 0, nil, false, ErrChecksum
			}
		} else if r.state == "running" {
			if i != len(current)-1 || r.verified.Valid || r.step < 0 {
				return 0, nil, false, ErrChecksum
			}
			if previous && i == 0 {
				sum = fmt.Sprintf("%x", a.upgrade.Transition.Checksum)
				steps = len(a.upgrade.Transition.Steps)
			}
			if r.checksum != sum || r.step > steps {
				return 0, nil, false, ErrChecksum
			}
			// Fresh repair requires that exact original schema file, not a later snapshot.
			if bootstrap && r.checksum != fmt.Sprintf("%x", a.schema.Checksum) {
				return 0, nil, false, ErrChecksum
			}
			if repair != "" && repair != r.checksum {
				return 0, nil, false, ErrChecksum
			}
			if repair == "" {
				return 0, nil, false, ErrDirty
			}
			return last, &r, previous && i == 0, nil
		} else {
			return 0, nil, false, ErrChecksum
		}
		last = r.revision
	}
	if repair != "" {
		return 0, nil, false, ErrChecksum
	}
	return last, nil, false, nil
}
func (a mysqlArtifacts) validateShape(ctx context.Context, conn *sql.Conn, shape map[string]step, dynamic bool) error {
	m := a.manifest(shape)
	if err := validateSnapshot(ctx, conn, m); err != nil {
		return err
	}
	expected := len(shape)
	if dynamic {
		// Planned shards may be resumed by the owner; full publication/check uses
		// validateRevisionSnapshot, which additionally verifies their state/ranges.
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM telemetry_sample_shards WHERE state IN ('planned','active','retired')").Scan(&count); err != nil {
			return safeError(err)
		}
		expected += count
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
func (a mysqlArtifacts) postcondition(ctx context.Context, conn schemaQueryer, m artifactStepMetadata) (string, error) {
	if m.Kind == "data" {
		var value string
		if err := conn.QueryRowContext(ctx, m.VerifySQL).Scan(&value); err != nil {
			return "", safeError(err)
		}
		return digest([]byte(value)), nil
	}
	if m.Kind != "seed" {
		return schemaHashWithFingerprint(ctx, conn, step{Name: m.Object, Kind: m.Kind}, a.fingerprint)
	}
	_, values, err := snapshotRows(ctx, conn, m.Object, m.Columns)
	if err != nil {
		return "", safeError(err)
	}
	if len(values) == 0 {
		return "", nil
	}
	if m.Object == "scheduler_leadership" {
		var valid bool
		if err = conn.QueryRowContext(ctx, "SELECT COUNT(*)=1 FROM scheduler_leadership JOIN schema_revisions WHERE state='running' AND updated_at BETWEEN TIMESTAMPDIFF(MICROSECOND,'2000-01-01',started_at) AND TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))").Scan(&valid); err != nil {
			return "", safeError(err)
		}
		if !valid {
			return "", ErrSchema
		}
	}
	data, _ := json.Marshal(values)
	return digest(data), nil
}
func advanceMySQLStep(ctx context.Context, conn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, r mysqlReceipt, next int) error {
	changed, err := conn.ExecContext(ctx, "UPDATE schema_revisions SET step=? WHERE epoch=? AND revision=? AND state='running' AND step=?", next, r.epoch, r.revision, next-1)
	if err != nil {
		return safeError(err)
	}
	n, err := changed.RowsAffected()
	if err != nil {
		return safeError(err)
	}
	if n != 1 {
		return ErrChecksum
	}
	return nil
}
func (a mysqlArtifacts) executeSteps(ctx context.Context, conn *sql.Conn, r mysqlReceipt, steps []schemaartifact.Step, shape map[string]step, fresh bool) error {
	if fresh {
		for i := 0; i < r.step; i++ {
			m := a.meta(steps[i])
			if m.Kind == "seed" {
				actual, err := a.postcondition(ctx, conn, m)
				if err != nil {
					return err
				}
				if actual != m.After {
					return ErrSchema
				}
			}
		}
	}
	for i := r.step; i < len(steps); i++ {
		s := steps[i]
		m := a.meta(s)
		actual, err := a.postcondition(ctx, conn, m)
		if err != nil {
			return err
		}
		if m.Kind != "data" && actual != m.Before && actual != m.After {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
		expected := cloneShape(shape)
		if actual == m.After {
			applyShape(expected, m)
		}
		if err = a.validateShape(ctx, conn, expected, !fresh); err != nil {
			return err
		}
		if m.Kind == "seed" || m.Kind == "data" {
			// InnoDB data and its progress receipt commit together. An uncertain DML
			// commit is never blindly replayed; the durable step selects the branch.
			tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				return safeError(err)
			}
			func() {
				defer tx.Rollback()
				if m.CheckBeforeSQL != "" {
					var valid string
					if err = tx.QueryRowContext(ctx, m.CheckBeforeSQL).Scan(&valid); err != nil {
						err = safeError(err)
						return
					}
					if valid != "valid" {
						err = ErrSchema
						return
					}
				}
				if m.Kind == "data" || actual != m.After {
					_, err = tx.ExecContext(ctx, string(s.SQL))
					if err != nil {
						err = safeError(err)
						return
					}
				}
				var after string
				after, err = a.postcondition(ctx, tx, m)
				if err != nil {
					return
				}
				if after != m.After {
					err = ErrSchema
					return
				}
				if err = advanceMySQLStep(ctx, tx, r, i+1); err != nil {
					return
				}
				err = safeError(tx.Commit())
			}()
			if err != nil {
				return err
			}
		} else {
			if actual != m.After {
				if _, err = conn.ExecContext(ctx, string(s.SQL)); err != nil {
					return safeError(err)
				}
			}
			after, err := a.postcondition(ctx, conn, m)
			if err != nil {
				return err
			}
			if after != m.After {
				return ErrSchema
			}
			if err = advanceMySQLStep(ctx, conn, r, i+1); err != nil {
				return err
			}
		}
		applyShape(shape, m)
	}
	if err := a.validateShape(ctx, conn, shape, !fresh); err != nil {
		return err
	}
	if !fresh {
		if err := validateRevisionSnapshot(ctx, conn, a.manifest(shape)); err != nil {
			return err
		}
	}
	return nil
}
func startMySQLRevision(ctx context.Context, conn *sql.Conn, epoch, revision int64, sum string) error {
	_, err := conn.ExecContext(ctx, "INSERT INTO schema_revisions(epoch,revision,checksum,state,step) VALUES(?,?,?,'running',0)", epoch, revision, sum)
	return safeError(err)
}
func publishMySQLRevision(ctx context.Context, conn *sql.Conn, r mysqlReceipt, sum string, steps int) error {
	_, err := conn.ExecContext(ctx, "UPDATE schema_revisions SET checksum=?,step=?,state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE epoch=? AND revision=? AND state='running'", sum, steps, r.epoch, r.revision)
	return safeError(err)
}
func (a mysqlArtifacts) initialize(ctx context.Context, conn *sql.Conn, repair string, existing bool) error {
	sum := fmt.Sprintf("%x", a.schema.Checksum)
	if repair != "" && repair != sum {
		return ErrChecksum
	}
	m := a.meta(a.journal)
	if existing {
		var comment string
		if err := conn.QueryRowContext(ctx, "SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='schema_revisions'").Scan(&comment); err != nil {
			return safeError(err)
		}
		if repair == "" {
			return ErrDirty
		}
		if comment != artifactCommentPrefix+sum {
			return ErrChecksum
		}
		if err := a.validateShape(ctx, conn, map[string]step{objectKey(m): {Name: m.Object, Kind: m.Kind, SchemaHash: m.After}}, false); err != nil {
			return err
		}
	} else {
		// CREATE atomically proves the empty database's exact initialization file,
		// even if the process dies before inserting its first journal row.
		ddl := strings.TrimSuffix(string(a.journal.SQL), ";\n") + " COMMENT='" + artifactCommentPrefix + sum + "'"
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return safeError(err)
		}
	}
	if err := startMySQLRevision(ctx, conn, a.schema.Epoch, a.schema.Baseline.Number, sum); err != nil {
		return err
	}
	r := mysqlReceipt{epoch: a.schema.Epoch, revision: a.schema.Baseline.Number, checksum: sum, state: "running"}
	shape := map[string]step{objectKey(m): {Name: m.Object, Kind: m.Kind, SchemaHash: m.After}}
	if err := a.executeSteps(ctx, conn, r, a.schema.Baseline.Steps, shape, true); err != nil {
		return err
	}
	return publishMySQLRevision(ctx, conn, r, sum, len(a.schema.Baseline.Steps))
}
func (b *Backend) Migrate(ctx context.Context, repair string) (result error) {
	a, err := loadMySQLArtifacts()
	if err != nil {
		return err
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() {
		if conn != nil {
			result = errors.Join(result, releaseMigrationConnection(conn, lock))
		}
	}()
	count, err := databaseObjectCount(ctx, conn)
	if err != nil {
		return err
	}
	if count == 0 {
		return a.initialize(ctx, conn, repair, false)
	}
	journal, err := schemaHashWithFingerprint(ctx, conn, step{Name: "schema_revisions", Kind: "table"}, a.fingerprint)
	if err != nil {
		return err
	}
	if journal == "" {
		// ponytail: only the original bridge window may adopt genuine legacy
		// receipts. Keep its classification and execution under the same lock.
		if a.schema.Epoch != 1 || a.schema.Baseline.Number != 0 {
			return ErrChecksum
		}
		return b.migrateLegacyOn(ctx, conn, repair)
	}
	if journal != a.meta(a.journal).After {
		return ErrSchema
	}
	rows, err := readMySQLReceipts(ctx, conn)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if count == 1 {
			return a.initialize(ctx, conn, repair, true)
		}
		if a.schema.Epoch != 1 || a.schema.Baseline.Number != 0 {
			return ErrChecksum
		}
		return b.migrateLegacyOn(ctx, conn, repair)
	}
	if err = validateArtifactJournalComment(ctx, conn, rows[0].checksum); err != nil {
		return err
	}
	head, r, transition, err := a.admit(rows, repair)
	if err != nil {
		return err
	}
	if r != nil && rows[0].epoch == a.schema.Epoch && rows[0].revision == r.revision {
		shape := map[string]step{}
		jm := a.meta(a.journal)
		applyShape(shape, jm)
		for i := 0; i < r.step; i++ {
			applyShape(shape, a.meta(a.schema.Baseline.Steps[i]))
		}
		if err = a.executeSteps(ctx, conn, *r, a.schema.Baseline.Steps, shape, true); err != nil {
			return err
		}
		return publishMySQLRevision(ctx, conn, *r, r.checksum, len(a.schema.Baseline.Steps))
	}
	if transition {
		var shape map[string]step
		_ = json.Unmarshal(a.upgrade.Previous.Receipt.Metadata, &shape)
		rev := *a.upgrade.Transition
		if r == nil {
			if err = a.validateShape(ctx, conn, shape, true); err != nil {
				return err
			}
			if err = startMySQLRevision(ctx, conn, a.schema.Epoch, 0, fmt.Sprintf("%x", rev.Checksum)); err != nil {
				return err
			}
			r = &mysqlReceipt{epoch: a.schema.Epoch, revision: 0, checksum: fmt.Sprintf("%x", rev.Checksum), state: "running"}
		} else {
			for i := 0; i < r.step; i++ {
				applyShape(shape, a.meta(rev.Steps[i]))
			}
		}
		if err = a.executeSteps(ctx, conn, *r, rev.Steps, shape, false); err != nil {
			return err
		}
		if err = publishMySQLRevision(ctx, conn, *r, r.checksum, len(rev.Steps)); err != nil {
			return err
		}
		head = 0
		r = nil
	}
	if r == nil {
		if err = validateRevisionSnapshot(ctx, conn, a.shapes[head]); err != nil {
			return err
		}
	}
	for _, rev := range a.upgrade.Revisions {
		if rev.Number <= head {
			continue
		}
		shape := shapeOf(a.shapes[rev.Number-1])
		if r == nil {
			if err = startMySQLRevision(ctx, conn, a.schema.Epoch, rev.Number, fmt.Sprintf("%x", rev.Checksum)); err != nil {
				return err
			}
			r = &mysqlReceipt{epoch: a.schema.Epoch, revision: rev.Number, checksum: fmt.Sprintf("%x", rev.Checksum), state: "running"}
		} else {
			for i := 0; i < r.step; i++ {
				applyShape(shape, a.meta(rev.Steps[i]))
			}
		}
		if err = a.executeSteps(ctx, conn, *r, rev.Steps, shape, false); err != nil {
			return err
		}
		if err = publishMySQLRevision(ctx, conn, *r, r.checksum, len(rev.Steps)); err != nil {
			return err
		}
		r = nil
	}
	return validateRevisionSnapshot(ctx, conn, a.shapes[len(a.shapes)-1])
}
func (b *Backend) artifactSnapshotOn(ctx context.Context, conn *sql.Conn) (manifest, error) {
	a, err := loadMySQLArtifacts()
	if err != nil {
		return manifest{}, err
	}
	rows, err := readMySQLReceipts(ctx, conn)
	if err != nil {
		return manifest{}, err
	}
	if len(rows) == 0 {
		return manifest{}, ErrChecksum
	}
	if err = validateArtifactJournalComment(ctx, conn, rows[0].checksum); err != nil {
		return manifest{}, err
	}
	head, r, transition, err := a.admit(rows, "")
	if err != nil {
		return manifest{}, err
	}
	if r != nil || transition || head != int64(len(a.upgrade.Revisions)) {
		return manifest{}, ErrChecksum
	}
	m := a.shapes[head]
	return m, validateSnapshot(ctx, conn, m)
}
func (b *Backend) ValidateSchema(ctx context.Context) (result error) {
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		return err
	}
	defer func() {
		if conn != nil {
			result = errors.Join(result, releaseMigrationConnection(conn, lock))
		}
	}()
	a, err := loadMySQLArtifacts()
	if err != nil {
		return err
	}
	journal, err := schemaHashWithFingerprint(ctx, conn, step{Name: "schema_revisions", Kind: "table"}, a.fingerprint)
	if err != nil {
		return err
	}
	if journal != "" && journal != a.meta(a.journal).After {
		return ErrSchema
	}
	var rows []mysqlReceipt
	if journal != "" {
		rows, err = readMySQLReceipts(ctx, conn)
		if err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		if a.schema.Epoch != 1 || a.schema.Baseline.Number != 0 {
			return ErrChecksum
		}
		if journal != "" {
			count, err := databaseObjectCount(ctx, conn)
			if err != nil {
				return err
			}
			if count == 1 {
				return ErrDirty
			}
		}
		return b.validateLegacySchemaOn(ctx, conn)
	}
	m, err := b.artifactSnapshotOn(ctx, conn)
	if err != nil {
		return err
	}
	return validateRevisionSnapshot(ctx, conn, m)
}

func validateArtifactJournalComment(ctx context.Context, conn *sql.Conn, sum string) error {
	var comment string
	if err := conn.QueryRowContext(ctx, "SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='schema_revisions'").Scan(&comment); err != nil {
		return safeError(err)
	}
	if comment != "" && comment != artifactCommentPrefix+sum {
		return ErrChecksum
	}
	return nil
}

// ArtifactChecksum identifies exactly the SQL whose running receipt may be resumed.
func ArtifactChecksum(engine Engine, kind string) (string, error) {
	if engine != MySQL {
		return "", ErrChecksum
	}
	a, err := loadMySQLArtifacts()
	if err != nil {
		return "", err
	}
	if kind == "schema" {
		return fmt.Sprintf("%x", a.schema.Checksum), nil
	}
	if kind == "upgrade" && len(a.upgrade.Revisions) > 0 {
		return fmt.Sprintf("%x", a.upgrade.Revisions[len(a.upgrade.Revisions)-1].Checksum), nil
	}
	return "", ErrChecksum
}
