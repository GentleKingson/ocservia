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

// PruneTelemetryPartitions retires one closed UTC month per run using the
// migration owner connection and advisory lock. Concurrent detach must run
// on the connection outside a transaction block.
func PruneTelemetryPartitions(ctx context.Context, pool *pgxpool.Pool) error {
	return withMigrationConnection(ctx, pool, func(conn *pgx.Conn) error {
		var owner bool
		if err := conn.QueryRow(ctx, `SELECT pg_has_role(relowner,'USAGE') FROM pg_class WHERE oid='public.telemetry_samples'::regclass`).Scan(&owner); err != nil {
			return err
		}
		if !owner {
			return errors.New("telemetry partition retention requires the table owner")
		}
		var name string
		var oid uint32
		err := conn.QueryRow(ctx, `SELECT partition_name,relation_oid::oid FROM public.telemetry_partition_retirements ORDER BY partition_name LIMIT 1`).Scan(&name, &oid)
		if errors.Is(err, pgx.ErrNoRows) {
			// Extra five minutes covers Controller/DB clock skew at the raw
			// retention boundary. A durable marker identifies our exact child
			// if the owner process exits between detach and drop.
			err = conn.QueryRow(ctx, `INSERT INTO public.telemetry_partition_retirements(partition_name,relation_oid)
				SELECT c.relname,c.oid::regclass FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
				WHERE i.inhparent='public.telemetry_samples'::regclass AND c.relnamespace='public'::regnamespace
				AND CASE WHEN c.relname ~ '^telemetry_samples_[1-9][0-9]{3}(0[1-9]|1[0-2])$'
					THEN to_date(substr(c.relname,19),'YYYYMM')+interval '1 month' <= (now() AT TIME ZONE 'UTC')-interval '14 days 5 minutes'
					ELSE false END
				ORDER BY c.relname LIMIT 1 RETURNING partition_name,relation_oid::oid`).Scan(&name, &oid)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var attached, pending, partitionOfAnotherTable bool
		if err := conn.QueryRow(ctx, `SELECT COALESCE(i.inhparent='public.telemetry_samples'::regclass,false),
			COALESCE(i.inhdetachpending,false), i.inhparent IS NOT NULL AND i.inhparent IS DISTINCT FROM 'public.telemetry_samples'::regclass
			FROM pg_class c LEFT JOIN pg_inherits i ON i.inhrelid=c.oid
			WHERE c.oid=$1 AND c.relname=$2 AND c.relnamespace='public'::regnamespace`, oid, name).
			Scan(&attached, &pending, &partitionOfAnotherTable); err != nil {
			return fmt.Errorf("verify telemetry retirement target: %w", err)
		}
		if partitionOfAnotherTable {
			return errors.New("telemetry retirement target belongs to another table")
		}
		yearMonth, ok := strings.CutPrefix(name, "telemetry_samples_")
		start, err := time.Parse("200601", yearMonth)
		if err != nil || !ok {
			return fmt.Errorf("invalid telemetry retirement target %q", name)
		}
		child := pgx.Identifier{"public", name}.Sanitize()
		if attached {
			mode := "CONCURRENTLY"
			if pending {
				mode = "FINALIZE"
			}
			if _, err := conn.Exec(ctx, "ALTER TABLE public.telemetry_samples DETACH PARTITION "+child+" "+mode); err != nil {
				return fmt.Errorf("detach telemetry partition %s: %w", name, err)
			}
		}
		return finishTelemetryRetirement(ctx, conn, name, child, oid, start)
	})
}

func finishTelemetryRetirement(ctx context.Context, conn *pgx.Conn, name, child string, oid uint32, start time.Time) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx, `SET LOCAL TimeZone TO 'UTC'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.telemetry_rollup_queue WHERE bucket_at >= $1 AND bucket_at < $2`, start, start.AddDate(0, 1, 0)); err != nil {
		return err
	}
	// Backfill from the detached, quiescent child. If maintenance was offline,
	// this preserves all rollups still within retention before raw data is dropped.
	if _, err := tx.Exec(ctx, `WITH source AS (
		SELECT node_id,metric,sampled_at,value FROM `+child+`
		UNION ALL
		SELECT node_id,metric,sampled_at,value FROM public.telemetry_samples_default WHERE sampled_at >= $1 AND sampled_at < $2
	), five AS MATERIALIZED (
		SELECT node_id,metric,date_bin(interval '5 minutes',sampled_at,'2000-01-01 00:00:00+00'::timestamptz) bucket_at,
			count(*) sample_count,min(value) min_value,max(value) max_value,avg(value) avg_value
		FROM source WHERE sampled_at>=date_bin(interval '1 hour',now()-interval '13 months','2000-01-01 00:00:00+00'::timestamptz)
		GROUP BY 1,2,3
	), stored AS (
		INSERT INTO public.telemetry_rollups_5m(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value)
		SELECT * FROM five WHERE bucket_at>=now()-interval '90 days'
		ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value
	)
	INSERT INTO public.telemetry_rollups_1h(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value)
	SELECT node_id,metric,date_bin(interval '1 hour',bucket_at,'2000-01-01 00:00:00+00'::timestamptz),
		sum(sample_count),min(min_value),max(max_value),sum(avg_value::numeric*sample_count)/sum(sample_count)
	FROM five GROUP BY 1,2,3
	ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value`, start, start.AddDate(0, 1, 0)); err != nil {
		return fmt.Errorf("preserve rollups from %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `DROP TABLE `+child); err != nil {
		return fmt.Errorf("drop detached telemetry partition %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.telemetry_partition_retirements WHERE partition_name=$1 AND relation_oid::oid=$2`, name, oid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
