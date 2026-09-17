package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Fixed maintenance steps, not a registry: only rollout errors allow the
// remaining steps in the same pass to continue.
type maintenanceWork struct {
	users, rollouts, telemetry, certificates, audit func(context.Context) error
	evidence                                        func(context.Context, *coordination.Session) error
}

func (w maintenanceWork) run(sessionCtx context.Context, session *coordination.Session, started time.Time, concurrency int, logger *slog.Logger) error {
	runCtx, span := otel.Tracer("ocservia.useroperations").Start(sessionCtx, "user_operations.scheduler.run")
	if err := w.users(runCtx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scheduler run failed")
		span.End()
		return err
	}
	span.End()
	logger.InfoContext(sessionCtx, "user operations scheduler completed", "duration_ms", time.Since(started).Milliseconds(), "submission_limit", concurrency)
	if err := w.rollouts(sessionCtx); err != nil {
		logger.ErrorContext(sessionCtx, "advance agent rollouts", "error", err)
	}
	if err := w.telemetry(sessionCtx); err != nil {
		return err
	}
	certificateCtx, certificateSpan := otel.Tracer("ocservia.certificates").Start(sessionCtx, "certificates.maintenance.run")
	if err := w.certificates(certificateCtx); err != nil {
		certificateSpan.RecordError(err)
		certificateSpan.SetStatus(codes.Error, "certificate maintenance failed")
		certificateSpan.End()
		return err
	}
	certificateSpan.End()
	if err := w.audit(sessionCtx); err != nil {
		return err
	}
	if w.evidence != nil {
		if err := w.evidence(sessionCtx, session); err != nil {
			return err
		}
	}
	return nil
}

type schedulerLeader interface {
	WithSession(context.Context, func(context.Context, *coordination.Session) error) error
	Stop()
}

func runScheduler(ctx context.Context, leader schedulerLeader, ticks <-chan time.Time, work maintenanceWork, concurrency int, logger *slog.Logger) error {
	defer leader.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		err := leader.WithSession(ctx, func(sessionCtx context.Context, session *coordination.Session) error {
			return work.run(sessionCtx, session, started, concurrency, logger)
		})
		if err != nil {
			var cleanup *useroperations.EnforcementCleanupError
			cleanupFailed := errors.As(err, &cleanup)
			if cleanupFailed && ctx.Err() != nil {
				return ctx.Err()
			}
			// Renewal loss cancels the session, not the process. Retry on the next tick.
			leadershipLost := errors.Is(err, coordination.ErrLeadershipLost) ||
				errors.Is(err, coordination.ErrNotLeader) ||
				(ctx.Err() == nil && errors.Is(err, context.Canceled))
			if leadershipLost {
				logger.WarnContext(ctx, "maintenance session lost leadership", "alert_kind", "scheduler.leadership_lost", "error", err)
			} else if cleanupFailed {
				logger.ErrorContext(ctx, "policy enforcement cleanup failed; remaining maintenance skipped; retry on next tick", "alert_kind", "user_operations.cleanup_failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
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
