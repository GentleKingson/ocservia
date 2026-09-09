package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// These are new backend histories, not records of PostgreSQL migrations.
//
//go:embed mysql/manifest.json mariadb/manifest.json history/f6cd0e0/*.json mysql/000002.json mariadb/000002.json
var manifests embed.FS

type step struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	SQL        string `json:"sql"`
	Checksum   string `json:"checksum"`
	SchemaHash string `json:"schema_hash"`
}
type manifest struct {
	Version                 int               `json:"version"`
	ControllerSchema        int               `json:"controller_schema"`
	MinimumControllerSchema int               `json:"minimum_controller_schema"`
	Engine                  Engine            `json:"engine"`
	Steps                   []step            `json:"steps"`
	MetadataHashes          map[string]string `json:"metadata_hashes"`
}

var ErrDirty = errors.New("experimental database: migration in progress; inspect schema and explicitly repair with the manifest checksum")
var ErrChecksum = errors.New("experimental database: migration checksum/history mismatch")
var ErrSchema = errors.New("experimental database: schema differs from the pinned baseline")
var ErrUnlock = errors.New("experimental database: migration unlock not confirmed; connection discarded")

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func loadManifest(engine Engine) (manifest, string, error) {
	data, err := manifests.ReadFile(string(engine) + "/manifest.json")
	if err != nil {
		return manifest{}, "", ErrChecksum
	}
	return decodeManifest(engine, data)
}
func decodeManifest(engine Engine, data []byte) (manifest, string, error) {
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, "", ErrChecksum
	}
	if m.Engine != engine || m.Version != 1 || m.ControllerSchema != 34 || m.MinimumControllerSchema != 34 || len(m.Steps) == 0 {
		return m, "", ErrChecksum
	}
	seen := map[string]bool{}
	for _, s := range m.Steps {
		if seen[s.Name] || !identifier.MatchString(s.Name) || digest([]byte(s.SQL)) != s.Checksum {
			return m, "", ErrChecksum
		}
		if s.Kind != "table" && s.Kind != "trigger" && s.Kind != "seed" {
			return m, "", ErrChecksum
		}
		seen[s.Name] = true
	}
	return m, digest(data), nil
}
func ManifestChecksum(engine Engine) (string, error) {
	_, sum, err := loadRevision(engine)
	return sum, err
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var autoIncrement = regexp.MustCompile(` AUTO_INCREMENT=[0-9]+`)

func schemaHash(ctx context.Context, conn *sql.Conn, s step) (string, error) {
	var definition string
	switch s.Kind {
	case "table":
		var name string
		err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE `"+s.Name+"`").Scan(&name, &definition)
		if err != nil {
			var e *mysqldriver.MySQLError
			if errors.As(err, &e) && e.Number == 1146 {
				return "", nil
			}
			return "", safeError(err)
		}
		definition = autoIncrement.ReplaceAllString(definition, "")
	case "trigger":
		err := conn.QueryRowContext(ctx, `SELECT CONCAT(ACTION_TIMING,' ',EVENT_MANIPULATION,' ',EVENT_OBJECT_TABLE,' ',ACTION_STATEMENT) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE() AND TRIGGER_NAME=?`, s.Name).Scan(&definition)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", safeError(err)
		}
	case "seed":
		var count int
		query := ""
		switch s.Name {
		case "seed_roles":
			query = "SELECT count(*) FROM roles WHERE name IN ('Viewer','Operator','UserManager','ConfigManager','Auditor','SecurityAdmin','PlatformAdmin')"
		case "seed_scheduler":
			query = "SELECT count(*) FROM scheduler_leadership WHERE id=1"
		case "seed_upstream":
			query = "SELECT count(*) FROM upstream_sync_records"
		default:
			return "", ErrSchema
		}
		if err := conn.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return "", safeError(err)
		}
		if count == 0 {
			return "", nil
		}
		definition = fmt.Sprint(count)
	}
	return digest([]byte(definition)), nil
}

func discard(conn *sql.Conn) {
	// database/sql removes and closes the underlying driver connection when Raw
	// returns ErrBadConn. Conn.Close alone would return a session lock to the pool.
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}
func migrationConnection(ctx context.Context, b *Backend) (*sql.Conn, string, error) {
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		return nil, "", safeError(err)
	}
	var databaseName string
	if err = conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil {
		conn.Close()
		return nil, "", safeError(err)
	}
	name := "ocservia:" + digest([]byte(databaseName))[:48]
	var result sql.NullInt64
	err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 10)", name).Scan(&result)
	if err != nil || !result.Valid || result.Int64 != 1 {
		discard(conn)
		conn.Close()
		if err != nil {
			return nil, "", safeError(err)
		}
		return nil, "", errors.New("experimental database: migration lock not acquired")
	}
	return conn, name, nil
}
func releaseMigrationConnection(conn *sql.Conn, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer conn.Close()
	var result sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&result); err != nil || !result.Valid || result.Int64 != 1 {
		discard(conn)
		return ErrUnlock
	}
	return nil
}

const metadataDDL = `CREATE TABLE IF NOT EXISTS backend_migrations (
 singleton TINYINT PRIMARY KEY CHECK(singleton=1), engine VARBINARY(16) NOT NULL,
 manifest_checksum CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 version INT NOT NULL DEFAULT 0, dirty BOOLEAN NOT NULL DEFAULT TRUE,
 controller_schema INT NOT NULL DEFAULT 0, minimum_controller_schema INT NOT NULL DEFAULT 0,
 repair_count INT NOT NULL DEFAULT 0, updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB`
const stepsDDL = `CREATE TABLE IF NOT EXISTS backend_migration_steps (
 ordinal INT PRIMARY KEY, name VARBINARY(64) NOT NULL UNIQUE,
 checksum CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK(state IN ('running','verified')),
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), verified_at DATETIME(6) NULL
) ENGINE=InnoDB`

// Migrate refuses dirty histories by default. Repair is a reviewed forward-only
// resume, authorized by the exact manifest checksum, never a force-clean flag.
// DDL is autocommitted; each statement is journaled before execution and its
// postcondition is checked before the next step. No history row is fabricated.
func (b *Backend) migrateBaseline(ctx context.Context, conn *sql.Conn, m manifest, sum, repairChecksum string) error {
	var err error
	if repairChecksum != "" && repairChecksum != sum {
		return ErrChecksum
	}
	for _, ddl := range []string{metadataDDL, stepsDDL} {
		if _, err = conn.ExecContext(ctx, ddl); err != nil {
			return safeError(err)
		}
	}
	for _, table := range []string{"backend_migrations", "backend_migration_steps"} {
		actual, err := schemaHash(ctx, conn, step{Name: table, Kind: "table"})
		if err != nil {
			return err
		}
		if len(m.MetadataHashes[table]) != 64 || actual != m.MetadataHashes[table] {
			return ErrSchema
		}
	}
	var stored, engine string
	var version int
	var dirty bool
	err = conn.QueryRowContext(ctx, "SELECT engine,manifest_checksum,version,dirty FROM backend_migrations WHERE singleton=1").Scan(&engine, &stored, &version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		// Refuse adopting an existing schema as an empty initialization.
		var count int
		if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME NOT IN ('backend_migrations','backend_migration_steps')").Scan(&count); err != nil {
			return safeError(err)
		}
		if count != 0 {
			return ErrSchema
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO backend_migrations(singleton,engine,manifest_checksum) VALUES(1,?,?)", b.engine, sum); err != nil {
			return safeError(err)
		}
	} else {
		if err != nil {
			return safeError(err)
		}
		if engine != string(b.engine) || stored != sum || version < 0 || version > m.Version {
			return ErrChecksum
		}
		if dirty && repairChecksum == "" {
			return ErrDirty
		}
		if !dirty && version != m.Version {
			return ErrSchema
		}
	}
	if repairChecksum != "" && dirty {
		if _, err = conn.ExecContext(ctx, "UPDATE backend_migrations SET repair_count=repair_count+1,updated_at=CURRENT_TIMESTAMP(6) WHERE singleton=1"); err != nil {
			return safeError(err)
		}
	}
	rows, err := conn.QueryContext(ctx, "SELECT ordinal,name,checksum,state FROM backend_migration_steps ORDER BY ordinal")
	if err != nil {
		return safeError(err)
	}
	states := []string{}
	for rows.Next() {
		var ordinal int
		var stepName, checksum, state string
		if err = rows.Scan(&ordinal, &stepName, &checksum, &state); err != nil {
			rows.Close()
			return safeError(err)
		}
		if ordinal != len(states)+1 || ordinal > len(m.Steps) || stepName != m.Steps[ordinal-1].Name || checksum != m.Steps[ordinal-1].Checksum || (state != "running" && state != "verified") {
			rows.Close()
			return ErrChecksum
		}
		if len(states) > 0 && states[len(states)-1] != "verified" {
			rows.Close()
			return ErrChecksum
		}
		states = append(states, state)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return safeError(err)
	}
	if version == m.Version && (len(states) != len(m.Steps) || dirty) {
		return ErrSchema
	}
	for i, s := range m.Steps {
		if len(s.SchemaHash) != 64 {
			return ErrSchema
		}
		actual, err := schemaHash(ctx, conn, s)
		if err != nil {
			return err
		}
		if i < len(states) && states[i] == "verified" {
			if actual != s.SchemaHash {
				return fmt.Errorf("%w: %s", ErrSchema, s.Name)
			}
			continue
		}
		if i >= len(states) {
			if actual != "" {
				return fmt.Errorf("%w: unjournaled object %s", ErrSchema, s.Name)
			}
			if _, err = conn.ExecContext(ctx, "INSERT INTO backend_migration_steps(ordinal,name,checksum,state) VALUES(?,?,?,'running')", i+1, s.Name, s.Checksum); err != nil {
				return safeError(err)
			}
		}
		// On explicit repair, accept only the exact postcondition or absence.
		// Any partial/foreign object requires manual inspection, not IF NOT EXISTS.
		if actual == "" {
			if _, err = conn.ExecContext(ctx, s.SQL); err != nil {
				return fmt.Errorf("migration step %s: %w", s.Name, safeError(err))
			}
			actual, err = schemaHash(ctx, conn, s)
			if err != nil {
				return err
			}
		}
		if actual != s.SchemaHash {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
		if _, err = conn.ExecContext(ctx, "UPDATE backend_migration_steps SET state='verified',verified_at=CURRENT_TIMESTAMP(6) WHERE ordinal=?", i+1); err != nil {
			return safeError(err)
		}
	}
	if version == m.Version {
		return b.validateOn(ctx, conn, m, sum, 0)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return safeError(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO controller_schema_compatibility(singleton,current_schema,minimum_compatible_controller_schema) VALUES(1,?,?)", m.ControllerSchema, m.MinimumControllerSchema); err != nil {
		return safeError(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE backend_migrations SET version=?,dirty=FALSE,controller_schema=?,minimum_controller_schema=?,updated_at=CURRENT_TIMESTAMP(6) WHERE singleton=1", m.Version, m.ControllerSchema, m.MinimumControllerSchema); err != nil {
		return safeError(err)
	}
	return safeError(tx.Commit())
}

func (b *Backend) validateOn(ctx context.Context, conn *sql.Conn, m manifest, sum string, extraTables int) error {
	var tableCount, triggerCount int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()").Scan(&tableCount); err != nil {
		return safeError(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE()").Scan(&triggerCount); err != nil {
		return safeError(err)
	}
	expectedTables, expectedTriggers := 2+extraTables, 0
	for _, s := range m.Steps {
		if s.Kind == "table" {
			expectedTables++
		}
		if s.Kind == "trigger" {
			expectedTriggers++
		}
	}
	if tableCount != expectedTables || triggerCount != expectedTriggers {
		return ErrSchema
	}
	for _, name := range []string{"backend_migrations", "backend_migration_steps"} {
		actual, err := schemaHash(ctx, conn, step{Name: name, Kind: "table"})
		if err != nil {
			return err
		}
		if len(m.MetadataHashes[name]) != 64 || actual != m.MetadataHashes[name] {
			return ErrSchema
		}
	}
	var checksum, engine string
	var version, current, minimum int
	var dirty bool
	err := conn.QueryRowContext(ctx, "SELECT engine,manifest_checksum,version,dirty,controller_schema,minimum_controller_schema FROM backend_migrations WHERE singleton=1").Scan(&engine, &checksum, &version, &dirty, &current, &minimum)
	if err != nil {
		return safeError(err)
	}
	if dirty {
		return ErrDirty
	}
	if checksum != sum || engine != string(b.engine) || version != m.Version {
		return ErrChecksum
	}
	if current != m.ControllerSchema || minimum != m.MinimumControllerSchema {
		return ErrSchema
	}
	var c, min int
	if err = conn.QueryRowContext(ctx, "SELECT current_schema,minimum_compatible_controller_schema FROM controller_schema_compatibility WHERE singleton=1").Scan(&c, &min); err != nil {
		return safeError(err)
	}
	if c != current || min != minimum {
		return ErrSchema
	}
	rows, err := conn.QueryContext(ctx, "SELECT ordinal,name,checksum,state FROM backend_migration_steps ORDER BY ordinal")
	if err != nil {
		return safeError(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var ordinal int
		var name, checksum, state string
		if err = rows.Scan(&ordinal, &name, &checksum, &state); err != nil {
			return safeError(err)
		}
		if i >= len(m.Steps) || ordinal != i+1 || name != m.Steps[i].Name || checksum != m.Steps[i].Checksum || state != "verified" {
			return ErrChecksum
		}
		i++
	}
	if err = rows.Err(); err != nil {
		return safeError(err)
	}
	if i != len(m.Steps) {
		return ErrChecksum
	}
	return nil
}

func validateSnapshot(ctx context.Context, conn *sql.Conn, m manifest) error {
	for _, s := range m.Steps {
		actual, err := schemaHash(ctx, conn, s)
		if err != nil {
			return err
		}
		if actual != s.SchemaHash {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
	}
	return nil
}
