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

type Preflight func(context.Context, pgx.Tx, int64) error

const controllerSchemaCompatibilityMigrationVersion int64 = 29

type SchemaCompatibility struct {
	CurrentSchema                     int64
	MinimumCompatibleControllerSchema int64
}

func (c SchemaCompatibility) Validate() error {
	if c.CurrentSchema < 1 || c.MinimumCompatibleControllerSchema < 1 {
		return errors.New("schema compatibility metadata contains a non-positive version")
	}
	if c.MinimumCompatibleControllerSchema > c.CurrentSchema {
		return fmt.Errorf("schema compatibility minimum %d exceeds current schema %d", c.MinimumCompatibleControllerSchema, c.CurrentSchema)
	}
	return nil
}

func (c SchemaCompatibility) Allows(expectedSchema int64) bool {
	return expectedSchema >= c.MinimumCompatibleControllerSchema && expectedSchema <= c.CurrentSchema
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

func Migrate(ctx context.Context, pool *pgxpool.Pool, preflights ...Preflight) error {
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
		if err := applyMigration(ctx, conn, migration, preflights); err != nil {
			return err
		}
	}
	if len(migrations) > 0 && migrations[len(migrations)-1].Version >= controllerSchemaCompatibilityMigrationVersion {
		if _, err := validateStoredSchemaCompatibility(ctx, conn); err != nil {
			return fmt.Errorf("validate schema compatibility: %w", err)
		}
	}
	return nil
}

func ValidateCurrentSchema(ctx context.Context, pool *pgxpool.Pool) error {
	expectedSchema, err := LatestSchemaVersion()
	if err != nil {
		return err
	}
	_, err = ValidateControllerSchema(ctx, pool, expectedSchema)
	return err
}

func ValidateControllerSchema(ctx context.Context, pool *pgxpool.Pool, expectedSchema int64) (SchemaCompatibility, error) {
	if expectedSchema < 1 {
		return SchemaCompatibility{}, errors.New("expected schema version must be positive")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return SchemaCompatibility{}, fmt.Errorf("acquire schema validation connection: %w", err)
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return SchemaCompatibility{}, fmt.Errorf("begin schema validation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	known, err := loadMigrations()
	if err != nil {
		return SchemaCompatibility{}, err
	}
	applied, err := readAppliedMigrations(ctx, tx)
	if err != nil {
		return SchemaCompatibility{}, err
	}
	if err := validateControllerAppliedMigrations(known, applied); err != nil {
		return SchemaCompatibility{}, err
	}
	compatibility, err := validateStoredSchemaCompatibility(ctx, tx)
	if err != nil {
		return SchemaCompatibility{}, err
	}
	if !compatibility.Allows(expectedSchema) {
		return SchemaCompatibility{}, fmt.Errorf("schema compatibility does not allow Controller schema %d: supported range is %d through %d", expectedSchema, compatibility.MinimumCompatibleControllerSchema, compatibility.CurrentSchema)
	}
	if err := tx.Commit(ctx); err != nil {
		return SchemaCompatibility{}, fmt.Errorf("commit schema validation: %w", err)
	}
	return compatibility, nil
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
		"GRANT SELECT ON controller_schema_compatibility TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON workspaces, nodes, operations TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON enrollment_tokens, node_endpoint_keys, node_capabilities TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON node_bootstrap_tokens TO " + identifier,
		"GRANT SELECT, INSERT ON node_sealing_keys TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON node_trust_convergence TO " + identifier,
		"GRANT SELECT, INSERT ON audit_events TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON identities, auth_sessions TO " + identifier,
		"GRANT SELECT ON roles TO " + identifier,
		"GRANT SELECT, INSERT, DELETE ON role_bindings TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON approval_requests TO " + identifier,
		"GRANT SELECT, INSERT ON audit_checkpoints, break_glass_uses TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON security_alerts TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON local_slice_jobs TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON commands, outbox_events, command_attempts, node_command_leases, operation_events TO " + identifier,
		"GRANT SELECT, INSERT ON agent_command_results TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON privd_attestation_enrollment_credentials TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON node_privd_attestation_keys TO " + identifier,
		"GRANT SELECT, INSERT ON transport_events TO " + identifier,
		"GRANT UPDATE (transport_cursor_valid) ON transport_events TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON transport_event_cursor TO " + identifier,
		"GRANT SELECT, INSERT ON transport_event_quarantine TO " + identifier,
		"GRANT USAGE ON SEQUENCE transport_events_ingest_sequence_seq TO " + identifier,
		"GRANT USAGE ON SEQUENCE operation_events_sequence_seq TO " + identifier,
		"GRANT SELECT, INSERT ON telemetry_ingest_batches TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON node_observed_snapshots, node_sessions TO " + identifier,
		"GRANT DELETE ON node_sessions TO " + identifier,
		"GRANT SELECT, INSERT, DELETE ON node_ip_bans TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON desired_users, desired_groups TO " + identifier,
		"GRANT SELECT, INSERT, DELETE ON observed_users, observed_groups TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON desired_user_policies, user_policy_mutations, observed_user_usage, user_usage_cursors, scheduler_leases, user_policy_enforcements, batch_operations, batch_operation_items TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON scheduler_leadership TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON connection_owner_fencing TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON node_config_state TO " + identifier,
		"GRANT SELECT, INSERT ON config_plans TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON config_apply_operations TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON agent_upgrade_operations TO " + identifier,
		"GRANT SELECT, INSERT ON node_agent_upgrade_results TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON agent_rollouts, agent_rollout_nodes TO " + identifier,
		"GRANT SELECT ON upstream_sync_records TO " + identifier,
		"GRANT SELECT, INSERT ON telemetry_security_events, telemetry_samples TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON telemetry_rollups_5m, telemetry_rollups_1h TO " + identifier,
		"GRANT EXECUTE ON FUNCTION telemetry_ensure_month_partition(timestamptz) TO " + identifier,
		"GRANT EXECUTE ON FUNCTION telemetry_drop_expired_partitions(timestamptz) TO " + identifier,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("grant privileges to runtime role %q: %w", role, err)
		}
	}
	return nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readAppliedMigrations(ctx context.Context, db queryer) ([]appliedMigration, error) {
	rows, err := db.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
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
	return readCurrentSchemaVersion(ctx, pool)
}

func readCurrentSchemaVersion(ctx context.Context, db queryer) (int64, error) {
	var version int64
	err := db.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func readSchemaCompatibility(ctx context.Context, db queryer) (SchemaCompatibility, error) {
	var compatibility SchemaCompatibility
	err := db.QueryRow(ctx, `
		SELECT "current_schema", minimum_compatible_controller_schema
		FROM controller_schema_compatibility
		WHERE singleton
	`).Scan(&compatibility.CurrentSchema, &compatibility.MinimumCompatibleControllerSchema)
	if errors.Is(err, pgx.ErrNoRows) {
		return SchemaCompatibility{}, errors.New("schema compatibility metadata is missing")
	}
	if err != nil {
		return SchemaCompatibility{}, fmt.Errorf("read schema compatibility metadata: %w", err)
	}
	if err := compatibility.Validate(); err != nil {
		return SchemaCompatibility{}, err
	}
	return compatibility, nil
}

func validateStoredSchemaCompatibility(ctx context.Context, db queryer) (SchemaCompatibility, error) {
	compatibility, err := readSchemaCompatibility(ctx, db)
	if err != nil {
		return SchemaCompatibility{}, err
	}
	currentSchema, err := readCurrentSchemaVersion(ctx, db)
	if err != nil {
		return SchemaCompatibility{}, err
	}
	if currentSchema != compatibility.CurrentSchema {
		return SchemaCompatibility{}, fmt.Errorf("schema compatibility current schema %d does not match applied schema version %d", compatibility.CurrentSchema, currentSchema)
	}
	return compatibility, nil
}

func validateControllerAppliedMigrations(known []Migration, applied []appliedMigration) error {
	if len(known) == 0 {
		return errors.New("no embedded migrations")
	}
	if len(applied) < len(known) {
		return fmt.Errorf("database schema is behind: found %d of %d migrations", len(applied), len(known))
	}
	if err := validateAppliedMigrations(known, applied[:len(known)]); err != nil {
		return err
	}
	for index := len(known); index < len(applied); index++ {
		if applied[index].Version <= applied[index-1].Version {
			return fmt.Errorf("applied migrations do not form an ordered prefix: version %d follows version %d", applied[index].Version, applied[index-1].Version)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgxpool.Conn, migration Migration, preflights []Preflight) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.Version, err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	for _, preflight := range preflights {
		if err := preflight(ctx, tx, migration.Version); err != nil {
			return fmt.Errorf("preflight migration %d: %w", migration.Version, err)
		}
	}
	if migration.Version > controllerSchemaCompatibilityMigrationVersion {
		result, err := tx.Exec(ctx, `
			UPDATE controller_schema_compatibility
			SET "current_schema" = $1, minimum_compatible_controller_schema = $1
			WHERE singleton
		`, migration.Version)
		if err != nil {
			return fmt.Errorf("prepare schema compatibility for migration %d: %w", migration.Version, err)
		}
		if result.RowsAffected() != 1 {
			return fmt.Errorf("prepare schema compatibility for migration %d: metadata row is missing", migration.Version)
		}
	}
	if _, err := tx.Exec(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply migration %d: %w", migration.Version, err)
	}
	if migration.Version >= controllerSchemaCompatibilityMigrationVersion {
		compatibility, err := readSchemaCompatibility(ctx, tx)
		if err != nil {
			return fmt.Errorf("validate schema compatibility for migration %d: %w", migration.Version, err)
		}
		if compatibility.CurrentSchema != migration.Version {
			return fmt.Errorf("validate schema compatibility for migration %d: current schema is %d", migration.Version, compatibility.CurrentSchema)
		}
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
