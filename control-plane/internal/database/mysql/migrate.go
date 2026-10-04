package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	mysqldriver "github.com/go-sql-driver/mysql"
	"io/fs"
	"regexp"
	"strings"
	"time"
)

//go:embed mysql/schema.sql mysql/upgrade.sql
var embeddedArtifacts embed.FS

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type artifactFiles struct{ fs.FS }

func (f artifactFiles) ReadFile(name string) ([]byte, error) { return fs.ReadFile(f.FS, name) }

var artifactSources = artifactFiles{embeddedArtifacts}

type step struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	SchemaHash string `json:"schema_hash"`
}
type schemaShape struct {
	Steps       []step
	fingerprint func(string) string
}

var ErrDirty = errors.New("experimental database: migration in progress; inspect schema and explicitly repair with the exact SQL artifact checksum")
var ErrChecksum = errors.New("experimental database: migration checksum/history mismatch")
var ErrSchema = errors.New("experimental database: schema differs from the pinned artifact")
var ErrUnlock = errors.New("experimental database: migration unlock not confirmed; connection discarded")
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var autoIncrement = regexp.MustCompile(` AUTO_INCREMENT=[0-9]+`)

// Prior checkpoint fresh installs may retain this atomic provenance comment.
var snapshotCommentSuffix = regexp.MustCompile(` COMMENT='ocservia-snapshot:[0-9a-f]{64}'$`)

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func schemaHash(ctx context.Context, conn schemaQueryer, s step) (string, error) {
	return schemaHashWithFingerprint(ctx, conn, s, tableFingerprint)
}

func schemaHashWithFingerprint(ctx context.Context, conn schemaQueryer, s step, fingerprint func(string) string) (string, error) {
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
		if s.Name == "backend_schema_snapshot" {
			definition = snapshotCommentSuffix.ReplaceAllString(definition, "")
		}
		if s.Name == "schema_revisions" {
			definition = artifactCommentSuffix.ReplaceAllString(definition, "")
		}
	case "trigger":
		err := conn.QueryRowContext(ctx, `SELECT CONCAT(ACTION_TIMING,' ',EVENT_MANIPULATION,' ',EVENT_OBJECT_TABLE,' ',ACTION_STATEMENT) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE() AND TRIGGER_NAME=?`, s.Name).Scan(&definition)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", safeError(err)
		}
	case "procedure", "function":
		err := conn.QueryRowContext(ctx, `SELECT CONCAT_WS('|',ROUTINE_TYPE,SQL_DATA_ACCESS,IS_DETERMINISTIC,SECURITY_TYPE,SQL_MODE,CHARACTER_SET_CLIENT,COLLATION_CONNECTION,DATABASE_COLLATION,ROUTINE_DEFINITION) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=DATABASE() AND ROUTINE_NAME=? AND ROUTINE_TYPE=?`, s.Name, strings.ToUpper(s.Kind)).Scan(&definition)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", safeError(err)
		}
		rows, err := conn.QueryContext(ctx, `SELECT CONCAT_WS('|',ORDINAL_POSITION,PARAMETER_MODE,PARAMETER_NAME,DTD_IDENTIFIER,COALESCE(CHARACTER_SET_NAME,''),COALESCE(COLLATION_NAME,'')) FROM information_schema.PARAMETERS WHERE SPECIFIC_SCHEMA=DATABASE() AND SPECIFIC_NAME=? ORDER BY ORDINAL_POSITION`, s.Name)
		if err != nil {
			return "", safeError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var parameter string
			if err = rows.Scan(&parameter); err != nil {
				return "", safeError(err)
			}
			definition += "\n" + parameter
		}
		if err = rows.Err(); err != nil {
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
	if s.Kind == "table" {
		return fingerprint(definition), nil
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
	// Appended upgrades can exceed one server wait interval. Keep each query
	// below the socket read timeout and the whole acquisition bounded.
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err = conn.QueryRowContext(lockCtx, "SELECT GET_LOCK(?, 10)", name).Scan(&result)
		if err != nil || !result.Valid || result.Int64 != 0 {
			break
		}
	}
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

func validateObjectCounts(ctx context.Context, conn *sql.Conn, m schemaShape, extraTables int) error {
	var tableCount, triggerCount, routineCount int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()").Scan(&tableCount); err != nil {
		return safeError(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE()").Scan(&triggerCount); err != nil {
		return safeError(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=DATABASE()").Scan(&routineCount); err != nil {
		return safeError(err)
	}
	expectedTables, expectedTriggers, expectedRoutines := extraTables, 0, 0
	for _, s := range m.Steps {
		if s.Kind == "table" {
			expectedTables++
		}
		if s.Kind == "trigger" {
			expectedTriggers++
		}
		if s.Kind == "procedure" || s.Kind == "function" {
			expectedRoutines++
		}
	}
	if tableCount != expectedTables || triggerCount != expectedTriggers || routineCount != expectedRoutines {
		return ErrSchema
	}
	return nil
}

func validateSnapshot(ctx context.Context, conn *sql.Conn, m schemaShape) error {
	for _, s := range m.Steps {
		// This seed initializes a mutable business table. Its receipt is
		// immutable, but subsequent synchronization records are not drift.
		if s.Kind == "seed" && s.Name == "seed_upstream" {
			continue
		}
		fingerprint := m.fingerprint
		if fingerprint == nil {
			fingerprint = tableFingerprint
		}
		actual, err := schemaHashWithFingerprint(ctx, conn, s, fingerprint)
		if err != nil {
			return err
		}
		if actual != s.SchemaHash {
			return fmt.Errorf("%w: %s", ErrSchema, s.Name)
		}
	}
	return nil
}

func databaseObjectCount(ctx context.Context, conn *sql.Conn) (int, error) {
	var n int
	err := conn.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE())+(SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=DATABASE())+(SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=DATABASE())+(SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA=DATABASE())`).Scan(&n)
	return n, safeError(err)
}

func validateRevisionSnapshot(ctx context.Context, conn *sql.Conn, snapshot schemaShape) error {
	var events int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA=DATABASE()").Scan(&events); err != nil {
		return safeError(err)
	}
	if events != 0 {
		return ErrSchema
	}
	if err := validateSnapshot(ctx, conn, snapshot); err != nil {
		return err
	}
	extra := 0
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
