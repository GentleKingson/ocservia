package migrations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationLockID int64 = 764057383691829796

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

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrateArtifacts(ctx, pool)
}

func withMigrationConnection(ctx context.Context, pool *pgxpool.Pool, run func(*pgxpool.Conn) error) (result error) {
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
	return run(conn)
}

func GrantRuntimePrivileges(ctx context.Context, pool *pgxpool.Pool, role string) error {
	identifier := pgx.Identifier{role}.Sanitize()
	statements := []string{
		"GRANT USAGE ON SCHEMA public TO " + identifier,
		"GRANT SELECT ON schema_revisions TO " + identifier,
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
