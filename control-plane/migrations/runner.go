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

func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		var version int
		if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
			return fmt.Errorf("inspect PostgreSQL server version: %w", err)
		}
		return validatePostgreSQLRelease(version, conn.PgConn().ParameterStatus("server_version"))
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
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool, preflights ...Preflight) error {
	known, err := loadMigrations()
	if err != nil {
		return err
	}
	current, err := loadSnapshot(known)
	if err != nil {
		return err
	}
	return migrate(ctx, pool, known, current, preflights)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, known []Migration, current snapshot, preflights []Preflight) (result error) {
	if _, _, err := baselineArtifact(current.SQL); err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = conn.Exec(lockCtx, "SELECT pg_advisory_lock($1)", migrationLockID)
	cancel()
	if err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = conn.Hijack().Close(closeCtx)
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockID).Scan(&unlocked)
		if err != nil || !unlocked {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			_ = conn.Hijack().Close(closeCtx)
			if result == nil {
				if err != nil {
					result = fmt.Errorf("unlock migrations: %w", err)
				} else {
					result = errors.New("migration lock ownership was lost")
				}
			}
			return
		}
		conn.Release()
	}()
	hasHistory, empty, err := databaseState(ctx, conn)
	if err != nil {
		return fmt.Errorf("classify PostgreSQL database: %w", err)
	}
	if empty {
		if err := initializeSnapshot(ctx, conn, current, known); err != nil {
			return err
		}
	} else if !hasHistory {
		return errors.New("nonempty PostgreSQL database has no recognized migration history; refusing initialization")
	}
	applied, err := readAppliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateAppliedMigrations(known, applied); err != nil {
		return err
	}
	for _, row := range applied {
		found := false
		for _, m := range known {
			if m.Version == row.Version {
				found = true
				break
			}
		}
		if !found {
			return errors.New("unsupported PostgreSQL history at checkpoint")
		}
	}
	recognized := false
	for _, row := range applied {
		for _, m := range known {
			if row.Version == m.Version {
				recognized = true
				break
			}
		}
	}
	if !recognized {
		return errors.New("PostgreSQL database has no known migration history; refusing adoption")
	}
	if err := validateOrigin(ctx, conn, known, applied); err != nil {
		return err
	}
	appliedVersions := make(map[int64]struct{}, len(applied))
	for _, m := range applied {
		appliedVersions[m.Version] = struct{}{}
	}
	for _, m := range known {
		if _, ok := appliedVersions[m.Version]; ok {
			continue
		}
		if err := applyMigration(ctx, conn, m, preflights); err != nil {
			return err
		}
	}
	if err := stampCheckpoint(ctx, conn, current, known); err != nil {
		return err
	}

	// Owner-only, repeated by --migrate-only to advance the provisioned horizon.
	if _, err := conn.Exec(ctx, `SELECT telemetry_ensure_month_partition(month AT TIME ZONE 'UTC') FROM generate_series(date_trunc('month',now() AT TIME ZONE 'UTC')-interval '1 month',date_trunc('month',now() AT TIME ZONE 'UTC')+interval '2 months',interval '1 month') AS month`); err != nil {
		return fmt.Errorf("provision telemetry partitions: %w", err)
	}
	return nil
}

func GrantRuntimePrivileges(ctx context.Context, pool *pgxpool.Pool, role string) error {
	identifier := pgx.Identifier{role}.Sanitize()
	statements := []string{
		"GRANT USAGE ON SCHEMA public TO " + identifier,
		"GRANT SELECT ON schema_migrations, schema_snapshot_origin, schema_revisions TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON workspaces, nodes, operations TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON enrollment_tokens, node_endpoint_keys, node_capabilities TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON node_bootstrap_tokens TO " + identifier,
		"GRANT SELECT, INSERT ON node_sealing_keys TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON node_trust_convergence TO " + identifier,
		"GRANT SELECT, INSERT ON audit_events TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE ON identities, auth_sessions, local_credentials TO " + identifier,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON local_auth_attempts TO " + identifier,
		"GRANT SELECT, INSERT ON local_auth_bootstrap TO " + identifier,
		"GRANT UPDATE (completion_pending,completed_at,approver_identity_id) ON local_auth_bootstrap TO " + identifier,
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
		"GRANT DELETE ON user_policy_enforcements TO " + identifier,
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
		"GRANT SELECT, INSERT, UPDATE ON telemetry_rollups_5m, telemetry_rollups_1h TO " + identifier,
		"GRANT EXECUTE ON FUNCTION telemetry_drop_expired_partitions(timestamptz) TO " + identifier,
		"REVOKE ALL ON FUNCTION telemetry_ensure_month_partition(timestamptz) FROM " + identifier,
		"REVOKE DELETE, TRUNCATE ON telemetry_rollups_5m, telemetry_rollups_1h FROM " + identifier,
		"GRANT EXECUTE ON FUNCTION telemetry_prune_rollups(timestamptz) TO " + identifier,
		// A column write privilege permits locking reads; the trigger still rejects runtime writes.
		"GRANT UPDATE(event_hash) ON audit_events TO " + identifier,
		"GRANT EXECUTE ON FUNCTION audit_compact_detail(uuid,bytea,text,bytea,timestamptz) TO " + identifier,
		"GRANT EXECUTE ON FUNCTION security_compact_details(timestamptz) TO " + identifier,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("grant privileges to runtime role %q: %w", role, err)
		}
	}
	var unsafe bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1,'telemetry_rollups_5m','DELETE,TRUNCATE') OR has_table_privilege($1,'telemetry_rollups_1h','DELETE,TRUNCATE')`, role).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe {
		return errors.New("runtime inherits unrestricted telemetry cleanup privileges; remove the inherited grant")
	}
	if err := pool.QueryRow(ctx, `SELECT has_function_privilege($1,'telemetry_ensure_month_partition(timestamptz)','EXECUTE')`, role).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe {
		return errors.New("runtime inherits telemetry partition DDL capability")
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
	present := make(map[int64]bool, len(applied))
	var highestKnown int64
	for _, migration := range applied {
		if _, ok := knownByVersion[migration.Version]; ok {
			present[migration.Version] = true
			if migration.Version > highestKnown {
				highestKnown = migration.Version
			}
		}
	}
	for _, migration := range known {
		if migration.Version <= highestKnown && !present[migration.Version] {
			return fmt.Errorf("migration history has a gap at known version %d", migration.Version)
		}
	}
	for _, migration := range applied {
		expected, ok := knownByVersion[migration.Version]
		if !ok {
			continue
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

func readCurrentSchemaVersion(ctx context.Context, db queryer) (int64, error) {
	var version int64
	err := db.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
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

func validatePostgreSQLVersion(version int) error {
	if version/10000 != 18 {
		return fmt.Errorf("PostgreSQL requires server major version 18, got %d", version/10000)
	}
	return nil
}

func validatePostgreSQLRelease(version int, release string) error {
	if err := validatePostgreSQLVersion(version); err != nil {
		return err
	}
	if strings.Contains(release, "beta") || strings.Contains(release, "rc") || strings.Contains(release, "devel") {
		return errors.New("PostgreSQL requires a stable 18.x release")
	}
	return nil
}

func rollbackMigration(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
