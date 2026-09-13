package postgres

import (
	"context"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

func (b *Backend) ValidateTelemetryRuntime(ctx context.Context) error {
	var safe bool
	err := b.QueryRow(ctx, `SELECT NOT has_schema_privilege(current_user,'public','CREATE') AND NOT has_table_privilege(current_user,'telemetry_rollups_5m','DELETE,TRUNCATE') AND NOT has_table_privilege(current_user,'telemetry_rollups_1h','DELETE,TRUNCATE') AND has_function_privilege(current_user,'telemetry_prune_rollups(timestamptz)','EXECUTE') AND has_function_privilege(current_user,'telemetry_drop_expired_partitions(timestamptz)','EXECUTE') AND NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&safe)
	if err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("runtime telemetry maintenance privileges are missing or unrestricted: %w", database.ErrPermission)
	}
	if err := b.QueryRow(ctx, `SELECT NOT has_function_privilege(current_user,'telemetry_ensure_month_partition(timestamptz)','EXECUTE')`).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("runtime has indirect telemetry partition DDL capability: %w", database.ErrPermission)
	}
	if err := b.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM generate_series(date_trunc('month',(now()-interval '14 days') AT TIME ZONE 'UTC'),date_trunc('month',(now()+interval '5 minutes') AT TIME ZONE 'UTC'),interval '1 month') month WHERE NOT EXISTS(SELECT 1 FROM pg_inherits WHERE inhparent='public.telemetry_samples'::regclass AND inhrelid=to_regclass('public.telemetry_samples_'||to_char(month,'YYYYMM'))))`).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("required telemetry month is not provisioned: %w", database.ErrConstraint)
	}
	return nil
}
