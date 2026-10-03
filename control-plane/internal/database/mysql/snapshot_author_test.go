package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Generation always replays immutable history into a disposable database. Check
// mode compares bytes without rewriting either checked-in artifact.
func TestCurrentSnapshotGeneration(t *testing.T) {
	output := os.Getenv("PR02_SNAPSHOT_DIRECTORY")
	if output == "" && os.Getenv("PR02_SNAPSHOT_CHECK") != "yes" {
		t.Skip("explicit snapshot generation/check only")
	}
	b, _ := historicalFixture(t, false)
	ctx := context.Background()
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMigrationConnection(conn, lock)
	chain, err := loadRevisionChain(MySQL)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.migrateChainOn(ctx, conn, chain, ""); err != nil {
		t.Fatal(err)
	}
	a, err := generateSnapshot(ctx, conn, chain)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(a.schemaSnapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	a.sum = digest(data)
	for _, s := range a.Statements {
		if s.RoundtripHash != "" {
			roundtripFingerprints[s.RoundtripHash] = s.SchemaHash
		}
	}
	// A separate empty database executes only the final SQL, never the replay.
	direct, _, _ := fixture(t)
	fresh, freshLock, e := migrationConnection(ctx, direct)
	if e != nil {
		t.Fatal(e)
	}
	defer releaseMigrationConnection(fresh, freshLock)
	if e = initializeSnapshot(ctx, fresh, a, nil, "", nil, ""); e != nil {
		t.Fatal("independent snapshot initialization", e)
	}
	root, _, e := loadManifest(MySQL)
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range chain {
		root = revisedSnapshot(root, r.Parents[a.Parent])
	}
	if e = validateRevisionSnapshot(ctx, fresh, root); e != nil {
		t.Fatal("independent snapshot equivalence", e)
	}

	if output == "" {
		if err = checkSnapshotArtifacts(a, data); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err = os.MkdirAll(output, 0755); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(filepath.Join(output, "schema.snapshot.json"))
	if err == nil && !bytes.Equal(previous, data) {
		var old schemaSnapshot
		if json.Unmarshal(previous, &old) != nil || old.Covered >= a.Covered {
			t.Fatal("refusing to replace same-coverage snapshot; append a revision first")
		}
		archive := filepath.Join(output, "snapshots")
		if err = os.MkdirAll(archive, 0755); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(archive, digest(previous)+".json")
		if existing, e := os.ReadFile(name); e == nil {
			if !bytes.Equal(existing, previous) {
				t.Fatal("immutable snapshot descriptor conflict")
			}
		} else if os.IsNotExist(e) {
			file, e := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if e != nil {
				t.Fatal(e)
			}
			_, writeErr := file.Write(previous)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatal(writeErr, closeErr)
			}
		} else {
			t.Fatal(e)
		}
	}
	for name, content := range map[string][]byte{"schema.sql": a.sql, "schema.snapshot.json": data} {
		if err = os.WriteFile(filepath.Join(output, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

var snapshotDefiner = regexp.MustCompile("DEFINER=`[^`]+`@`[^`]+` ")

func generateSnapshot(ctx context.Context, conn *sql.Conn, chain []revisionArtifact) (snapshotArtifact, error) {
	_, parent, err := loadManifest(MySQL)
	if err != nil {
		return snapshotArtifact{}, err
	}
	a := snapshotArtifact{schemaSnapshot: schemaSnapshot{Format: 1, Engine: MySQL, Covered: chain[len(chain)-1].Version, Parent: parent, RevisionChecksum: chain[len(chain)-1].sum}}
	a.HistoryChecksum, err = historyChecksum(chain, parent, a.Covered)
	if err != nil {
		return a, err
	}
	add := func(name, kind, statement, hash string, columns []string) {
		a.Statements = append(a.Statements, schemaStatement{Name: name, Kind: kind, Offset: len(a.sql), Length: len(statement), Checksum: digest([]byte(statement)), SchemaHash: hash, Columns: columns})
		a.sql = append(a.sql, []byte(statement+";\n")...)
	}
	rows, err := conn.QueryContext(ctx, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() ORDER BY TABLE_NAME")
	if err != nil {
		return a, err
	}
	var pending []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return a, err
		}
		pending = append(pending, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	// Metadata starts with the atomic origin marker and its journal.
	order := []string{"backend_schema_snapshot", "backend_schema_snapshot_steps"}
	created := map[string]bool{order[0]: true, order[1]: true}
	for len(created) < len(pending) {
		progress := false
		for _, name := range pending {
			if created[name] {
				continue
			}
			deps, err := conn.QueryContext(ctx, "SELECT DISTINCT REFERENCED_TABLE_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND REFERENCED_TABLE_NAME IS NOT NULL", name)
			if err != nil {
				return a, err
			}
			ready := true
			for deps.Next() {
				var n string
				if err = deps.Scan(&n); err != nil {
					deps.Close()
					return a, err
				}
				if n != name && !created[n] {
					ready = false
				}
			}
			err = deps.Err()
			deps.Close()
			if err != nil {
				return a, err
			}
			if ready {
				created[name] = true
				order = append(order, name)
				progress = true
			}
		}
		if !progress {
			return a, fmt.Errorf("cyclic snapshot foreign keys")
		}
	}
	for _, name := range order {
		var returned, ddl string
		if err = conn.QueryRowContext(ctx, "SHOW CREATE TABLE `"+name+"`").Scan(&returned, &ddl); err != nil {
			return a, err
		}
		ddl = autoIncrement.ReplaceAllString(ddl, "")
		if name == "backend_schema_snapshot" {
			ddl = snapshotCommentSuffix.ReplaceAllString(ddl, "")
		}
		roundtripHash := digest([]byte(strings.ReplaceAll(ddl, ">= -211813488000000000", ">= -(211813488000000000)")))
		// SHOW CREATE prints inherited column COLLATE without CHARACTER SET.
		// Re-executing that explicit COLLATE makes MySQL print an extra charset.
		// Omit only the inherited, table-default collation so the resulting SHOW
		// CREATE remains byte-identical to the immutable historical fingerprint.
		if strings.HasSuffix(ddl, "DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin") {
			lines := strings.Split(ddl, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "  `") && !strings.Contains(line, "CHARACTER SET") {
					lines[i] = strings.ReplaceAll(line, " COLLATE utf8mb4_0900_bin", "")
				}
			}
			ddl = strings.Join(lines, "\n")
		}
		hash, err := schemaHash(ctx, conn, step{Name: name, Kind: "table"})
		if err != nil {
			return a, err
		}
		add(name, "table", ddl, hash, nil)
		if roundtripHash != hash {
			a.Statements[len(a.Statements)-1].RoundtripHash = roundtripHash
		}
	}
	// Capture actual seed contents, including revision-introduced guards. Never
	// copy execution receipts from the historical replay into a fresh database.
	for _, name := range order {
		if strings.HasPrefix(name, "backend_") || name == "time_migration_decisions" {
			continue
		}
		var seedColumns []string
		// updated_at is initialization time, not a reproducible historical seed.
		if name == "scheduler_leadership" {
			seedColumns = []string{"id", "instance_id", "incarnation", "epoch", "lease_until"}
		}
		columns, values, err := snapshotRows(ctx, conn, name, seedColumns)
		if err != nil {
			return a, err
		}
		if len(values) == 0 {
			continue
		}
		quoted := make([]string, len(columns))
		for i, c := range columns {
			quoted[i] = "`" + c + "`"
		}
		var literals []string
		for _, row := range values {
			var cells []string
			for _, cell := range row {
				if cell == nil {
					cells = append(cells, "NULL")
				} else {
					cells = append(cells, "CAST(X'"+*cell+"' AS BINARY)")
				}
			}
			if name == "scheduler_leadership" {
				cells = append(cells, "TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))")
			}
			literals = append(literals, "("+strings.Join(cells, ",")+")")
		}
		if name == "scheduler_leadership" {
			quoted = append(quoted, "`updated_at`")
		}
		data, _ := json.Marshal(values)
		add(name, "seed", "INSERT INTO `"+name+"` ("+strings.Join(quoted, ",")+") VALUES "+strings.Join(literals, ","), digest(data), columns)
	}
	for _, kind := range []string{"function", "procedure", "trigger"} {
		query := "SELECT ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=DATABASE() AND ROUTINE_TYPE=? ORDER BY ROUTINE_NAME"
		args := []any{strings.ToUpper(kind)}
		if kind == "trigger" {
			query = "SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE() ORDER BY TRIGGER_NAME"
			args = nil
		}
		names, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return a, err
		}
		var list []string
		for names.Next() {
			var n string
			if err = names.Scan(&n); err != nil {
				names.Close()
				return a, err
			}
			list = append(list, n)
		}
		err = names.Err()
		names.Close()
		if err != nil {
			return a, err
		}
		for _, name := range list {
			r, err := conn.QueryContext(ctx, "SHOW CREATE "+strings.ToUpper(kind)+" `"+name+"`")
			if err != nil {
				return a, err
			}
			columns, err := r.Columns()
			if err != nil {
				r.Close()
				return a, err
			}
			values := make([]sql.NullString, len(columns))
			dest := make([]any, len(columns))
			for i := range dest {
				dest[i] = &values[i]
			}
			if !r.Next() {
				r.Close()
				return a, ErrSchema
			}
			err = r.Scan(dest...)
			r.Close()
			if err != nil {
				return a, err
			}
			ddl := ""
			for i, c := range columns {
				if strings.HasPrefix(c, "Create ") || c == "SQL Original Statement" {
					ddl = values[i].String
				}
			}
			if ddl == "" {
				return a, ErrSchema
			}
			ddl = snapshotDefiner.ReplaceAllString(ddl, "")
			hash, err := schemaHash(ctx, conn, step{Name: name, Kind: kind})
			if err != nil {
				return a, err
			}
			add(name, kind, ddl, hash, nil)
		}
	}
	a.SchemaChecksum = digest(a.sql)
	return a, nil
}

func checkSnapshotArtifacts(a snapshotArtifact, data []byte) error {
	for path, want := range map[string][]byte{"mysql/schema.sql": a.sql, "mysql/schema.snapshot.json": data} {
		got, err := manifests.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("snapshot drift: %s: %w", path, ErrChecksum)
		}
	}
	return nil
}

func TestSnapshotGenerationDeterminism(t *testing.T) {
	ctx := context.Background()
	chain, err := loadRevisionChain(MySQL)
	if err != nil {
		t.Fatal(err)
	}
	var previous []byte
	for range 2 {
		b, _ := historicalFixture(t, false)
		conn, lock, err := migrationConnection(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if err = b.migrateChainOn(ctx, conn, chain, ""); err != nil {
			t.Fatal(err)
		}
		a, err := generateSnapshot(ctx, conn, chain)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(a.schemaSnapshot)
		if previous != nil && !bytes.Equal(previous, data) {
			t.Fatal("two clean historical replays generated different snapshot descriptors")
		}
		previous = data
		if err = releaseMigrationConnection(conn, lock); err != nil {
			t.Fatal(err)
		}
	}
}
