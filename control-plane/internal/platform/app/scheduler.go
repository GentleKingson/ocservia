package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

const defaultSchedulerStepTimeout = 25 * time.Second

// Maintenance steps execute in a fixed order and share the Controller lifecycle.
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

func (w maintenanceWork) run(ctx context.Context, concurrency int, logger *slog.Logger, lastSuccess map[string]time.Time) error {
	steps := [...]struct {
		name     string
		fn       func(context.Context) error
		isolated bool
		tracer   string
		span     string
	}{
		{name: "users", fn: w.users, tracer: "ocservia.useroperations", span: "user_operations.scheduler.run"},
		{name: "rollouts", fn: w.rollouts, isolated: true},
		{name: "telemetry", fn: w.telemetry, isolated: true},
		{name: "certificates", fn: w.certificates, isolated: true, tracer: "ocservia.certificates", span: "certificates.maintenance.run"},
		{name: "audit", fn: w.audit},
		{name: "retention", fn: w.retention, isolated: true},
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if step.fn == nil {
			continue
		}
		started := time.Now()
		stepCtx, cancel := w.stepContext(ctx)
		err := runMaintenanceStep(stepCtx, step.fn, step.tracer, step.span)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		duration := time.Since(started).Milliseconds()
		if err == nil {
			lastSuccess[step.name] = time.Now().UTC()
			if step.name == "users" {
				logger.InfoContext(ctx, "user operations scheduler completed", "duration_ms", duration, "submission_limit", concurrency)
			}
			continue
		}
		var cleanup *useroperations.EnforcementCleanupError
		isolate := step.isolated || errors.Is(err, database.ErrDeadlock) || errors.Is(err, database.ErrSerialization)
		alert := "scheduler.step_failed"
		switch {
		case errors.Is(err, database.ErrCommitUnknown):
			alert, isolate = "scheduler.commit_unknown", false
		case errors.Is(err, database.ErrUnavailable):
			alert, isolate = "scheduler.database_unavailable", false
		case step.name == "users" && errors.As(err, &cleanup) && errors.Is(cleanup, database.ErrPermission):
			alert, isolate = "user_operations.cleanup_failed", true
		}
		logger.ErrorContext(ctx, "background maintenance step failed", "alert_kind", alert, "step", step.name, "error", err,
			"duration_ms", duration, "last_success_at", lastSuccess[step.name])
		if !isolate {
			return fmt.Errorf("maintenance %s: %w", step.name, err)
		}
	}
	return nil
}

func runMaintenanceStep(ctx context.Context, fn func(context.Context) error, tracer, name string) error {
	if name == "" {
		return fn(ctx)
	}
	ctx, span := otel.Tracer(tracer).Start(ctx, name)
	defer span.End()
	if err := fn(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "maintenance step failed")
		return err
	}
	return nil
}

func runScheduler(ctx context.Context, ticks <-chan time.Time, work maintenanceWork, concurrency int, logger *slog.Logger) error {
	lastSuccess := make(map[string]time.Time)
	for {
		if err := work.run(ctx, concurrency, logger, lastSuccess); err != nil &&
			!errors.Is(err, database.ErrUnavailable) && !errors.Is(err, database.ErrCommitUnknown) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
		}
	}
}
