package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Fixed maintenance steps, not a registry: only rollout errors allow the
// remaining steps in the same pass to continue.
type maintenanceWork struct {
	users, rollouts, telemetry, certificates, audit func(context.Context) error
	retention                                       func(context.Context) error
}

func (w maintenanceWork) run(ctx context.Context, started time.Time, concurrency int, logger *slog.Logger) error {
	runCtx, span := otel.Tracer("ocservia.useroperations").Start(ctx, "user_operations.scheduler.run")
	if err := w.users(runCtx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scheduler run failed")
		span.End()
		return err
	}
	span.End()
	logger.InfoContext(ctx, "user operations scheduler completed", "duration_ms", time.Since(started).Milliseconds(), "submission_limit", concurrency)
	if err := w.rollouts(ctx); err != nil {
		logger.ErrorContext(ctx, "advance agent rollouts", "error", err)
	}
	if err := w.telemetry(ctx); err != nil {
		return err
	}
	certificateCtx, certificateSpan := otel.Tracer("ocservia.certificates").Start(ctx, "certificates.maintenance.run")
	if err := w.certificates(certificateCtx); err != nil {
		certificateSpan.RecordError(err)
		certificateSpan.SetStatus(codes.Error, "certificate maintenance failed")
		certificateSpan.End()
		return err
	}
	certificateSpan.End()
	if err := w.audit(ctx); err != nil {
		return err
	}
	if w.retention != nil {
		if err := w.retention(ctx); err != nil {
			return err
		}
	}
	return nil
}

func runScheduler(ctx context.Context, ticks <-chan time.Time, work maintenanceWork, concurrency int, logger *slog.Logger) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		if err := work.run(ctx, started, concurrency, logger); err != nil {
			var cleanup *useroperations.EnforcementCleanupError
			cleanupFailed := errors.As(err, &cleanup)
			if cleanupFailed && ctx.Err() != nil {
				return ctx.Err()
			}
			if cleanupFailed {
				logger.ErrorContext(ctx, "policy enforcement cleanup failed; remaining maintenance skipped; retry on next tick", "alert_kind", "user_operations.cleanup_failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
			} else if errors.Is(err, database.ErrUnavailable) && ctx.Err() == nil {
				logger.ErrorContext(ctx, "maintenance database unavailable; remaining maintenance skipped; retry on next tick", "alert_kind", "scheduler.database_unavailable", "error", err, "duration_ms", time.Since(started).Milliseconds())
			} else {
				logger.ErrorContext(ctx, "user operations scheduler failed", "alert_kind", "user_operations.scheduler_failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
		}
	}
}
