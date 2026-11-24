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

const defaultSchedulerStepTimeout = 25 * time.Second

// Fixed maintenance steps: only users cleanup/DB-unavailable skips the rest of
// the pass, rollouts/telemetry/certificates/retention isolate per-step errors
// and timeouts, and audit failures remain fatal.
type maintenanceWork struct {
	users, rollouts, telemetry, certificates, audit func(context.Context) error
	retention                                       func(context.Context) error
	stepTimeout                                     time.Duration
}

func (w maintenanceWork) stepContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := w.stepTimeout
	if timeout <= 0 {
		timeout = defaultSchedulerStepTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (w maintenanceWork) runIsolated(ctx context.Context, name string, logger *slog.Logger, fn func(context.Context) error) error {
	if fn == nil {
		return nil
	}
	stepCtx, cancel := w.stepContext(ctx)
	defer cancel()
	err := fn(stepCtx)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, database.ErrUnavailable) {
		return err
	}
	logger.ErrorContext(ctx, "background maintenance step failed", "step", name, "error", err)
	return nil
}

func (w maintenanceWork) run(ctx context.Context, started time.Time, concurrency int, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	usersCtx, usersCancel := w.stepContext(ctx)
	runCtx, span := otel.Tracer("ocservia.useroperations").Start(usersCtx, "user_operations.scheduler.run")
	err := w.users(runCtx)
	usersCancel()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scheduler run failed")
		span.End()
		return err
	}
	span.End()
	logger.InfoContext(ctx, "user operations scheduler completed", "duration_ms", time.Since(started).Milliseconds(), "submission_limit", concurrency)
	if err := w.runIsolated(ctx, "rollouts", logger, w.rollouts); err != nil {
		return err
	}
	if err := w.runIsolated(ctx, "telemetry", logger, w.telemetry); err != nil {
		return err
	}
	if err := w.runIsolated(ctx, "certificates", logger, func(stepCtx context.Context) error {
		certificateCtx, certificateSpan := otel.Tracer("ocservia.certificates").Start(stepCtx, "certificates.maintenance.run")
		err := w.certificates(certificateCtx)
		if err != nil {
			certificateSpan.RecordError(err)
			certificateSpan.SetStatus(codes.Error, "certificate maintenance failed")
		}
		certificateSpan.End()
		return err
	}); err != nil {
		return err
	}
	auditCtx, auditCancel := w.stepContext(ctx)
	err = w.audit(auditCtx)
	auditCancel()
	if err != nil {
		return err
	}
	return w.runIsolated(ctx, "retention", logger, w.retention)
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
