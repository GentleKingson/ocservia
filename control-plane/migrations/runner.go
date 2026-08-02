package migrations

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.up.sql
var migrationFiles embed.FS

const migrationLockID int64 = 764057383691829796

type Migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum [sha256.Size]byte
}

type appliedMigration struct {
	Version  int64
	Name     string
	Checksum []byte
}

func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockID)
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version bigint PRIMARY KEY, name text NOT NULL, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migration metadata: %w", err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := readAppliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateAppliedMigrations(migrations, applied); err != nil {
		return err
	}
	appliedVersions := make(map[int64]struct{}, len(applied))
	for _, migration := range applied {
		appliedVersions[migration.Version] = struct{}{}
	}
	for _, migration := range migrations {
		if _, ok := appliedVersions[migration.Version]; ok {
			continue
		}
		if err := applyMigration(ctx, conn, migration); err != nil {
			return err
		}
	}
	return nil
}

func ValidateCurrentSchema(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire schema validation connection: %w", err)
	}
	defer conn.Release()
	known, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := readAppliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateAppliedMigrations(known, applied); err != nil {
		return err
	}
	if len(applied) != len(known) {
		return fmt.Errorf("database schema is behind: found %d of %d migrations", len(applied), len(known))
	}
	return nil
}

func LatestSchemaVersion() (int64, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	if len(migrations) == 0 {
		return 0, errors.New("no embedded migrations")
	}
	return migrations[len(migrations)-1].Version, nil
}

func GrantRuntimePrivileges(ctx context.Context, pool *pgxpool.Pool, role string) error {
	identifier := pgx.Identifier{role}.Sanitize()
	statements := []string{
		"GRANT USAGE ON SCHEMA public TO " + identifier,
		"GRANT SELECT ON schema_migrations TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON workspaces, nodes, operations TO " + identifier,
		"GRANT SELECT, INSERT ON audit_events TO " + identifier,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("grant privileges to runtime role %q: %w", role, err)
		}
	}
	return nil
}

func readAppliedMigrations(ctx context.Context, conn *pgxpool.Conn) ([]appliedMigration, error) {
	rows, err := conn.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	var applied []appliedMigration
	for rows.Next() {
		var migration appliedMigration
		if err := rows.Scan(&migration.Version, &migration.Name, &migration.Checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied = append(applied, migration)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

func validateAppliedMigrations(known []Migration, applied []appliedMigration) error {
	knownByVersion := make(map[int64]Migration, len(known))
	for _, migration := range known {
		knownByVersion[migration.Version] = migration
	}
	for index, migration := range applied {
		expected, ok := knownByVersion[migration.Version]
		if !ok {
			return fmt.Errorf("database schema version %d is unknown to this binary", migration.Version)
		}
		if migration.Version != known[index].Version {
			return fmt.Errorf("applied migrations do not form an ordered prefix: expected version %d before version %d", known[index].Version, migration.Version)
		}
		if migration.Name != expected.Name {
			return fmt.Errorf("migration %d name does not match the applied schema", migration.Version)
		}
		if !equalChecksum(migration.Checksum, expected.Checksum[:]) {
			return fmt.Errorf("migration %d checksum does not match the applied schema", migration.Version)
		}
	}
	return nil
}

func CurrentSchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var version int64
	err := pool.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func applyMigration(ctx context.Context, conn *pgxpool.Conn, migration Migration) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.Version, err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply migration %d: %w", migration.Version, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)", migration.Version, migration.Name, migration.Checksum[:]); err != nil {
		return fmt.Errorf("record migration %d: %w", migration.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %d: %w", migration.Version, err)
	}
	return nil
}

func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	result := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		parts := strings.SplitN(entry.Name(), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid migration name %q", entry.Name())
		}
		version, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid migration version %q: %w", parts[0], err)
		}
		data, err := migrationFiles.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		result = append(result, Migration{Version: version, Name: entry.Name(), SQL: string(data), Checksum: sha256.Sum256(data)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	for i := 1; i < len(result); i++ {
		if result[i-1].Version == result[i].Version {
			return nil, errors.New("duplicate migration version")
		}
	}
	return result, nil
}

func equalChecksum(left, right []byte) bool {
	return subtle.ConstantTimeCompare(left, right) == 1
}
