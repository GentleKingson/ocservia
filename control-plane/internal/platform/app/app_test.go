package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
)

func TestBootstrapPasswordPolicyBeforeStartup(t *testing.T) {
	for _, password := range []string{"nA6/X2k", "password", "12345678901234567890", "ocserviapassword"} {
		// No database or logger: policy rejection must precede startup side effects.
		err := Run(context.Background(), config.Config{BootstrapLocalAdmin: true, LocalBootstrapPassword: password}, BuildInfo{}, nil, nil, nil)
		if !errors.Is(err, auth.ErrPasswordPolicy) {
			t.Fatalf("bootstrap reached startup before policy rejection: %v", err)
		}
	}
	if err := validateBootstrapPasswords(config.Config{BootstrapLocalAdmin: true, LocalBootstrapPassword: "nA6/X2kZ"}); err != nil {
		t.Fatalf("eight-character bootstrap password rejected: %v", err)
	}
}

func TestOperationAuthIsNotEnabledBySimulator(t *testing.T) {
	if operationAuthEnabled(config.Config{LocalSimulator: true}) {
		t.Fatal("local simulator enabled operation authentication")
	}
	if !operationAuthEnabled(config.Config{DevAuth: true, LocalSimulator: true}) {
		t.Fatal("development authentication was not enabled")
	}
}

func TestTelemetryRetentionScheduleRetriesAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	err := runTelemetryRetentionSchedule(ctx, 10*time.Millisecond, 5*time.Millisecond, func(context.Context) error {
		calls++
		switch calls {
		case 1:
			return errors.New("transient database error")
		case 2:
			return nil
		default:
			cancel()
			return context.Canceled
		}
	}, nil)
	if err != nil {
		t.Fatalf("scheduled retention exited with error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 passes (error -> retry -> success -> cancel), got %d", calls)
	}
}

func TestSchedulerIsolatesNonFatalStepErrorsAndTimeouts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ticks := make(chan time.Time)
	var ran []string
	auditErr := errors.New("audit hash chain corrupted")

	pass := 0
	work := maintenanceWork{
		stepTimeout: 20 * time.Millisecond,
		users: func(context.Context) error {
			pass++
			ran = append(ran, "users")
			if pass == 2 {
				return &useroperations.EnforcementCleanupError{Err: errors.New("cleanup permission denied")}
			}
			return nil
		},
		rollouts: func(context.Context) error {
			ran = append(ran, "rollouts")
			return errors.New("rollout boom")
		},
		telemetry: func(stepCtx context.Context) error {
			ran = append(ran, "telemetry")
			<-stepCtx.Done()
			return stepCtx.Err()
		},
		certificates: func(context.Context) error {
			ran = append(ran, "certificates")
			return errors.New("cert boom")
		},
		audit: func(context.Context) error {
			ran = append(ran, "audit")
			if pass >= 3 {
				return auditErr
			}
			return nil
		},
		retention: func(context.Context) error {
			ran = append(ran, "retention")
			return errors.New("retention boom")
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- runScheduler(ctx, ticks, work, 1, logger)
	}()

	// Pass 1 ran immediately on entry; send tick for Pass 2 (users cleanup skips rest of pass) and Pass 3 (audit fatal).
	ticks <- time.Now()
	ticks <- time.Now()

	err := <-done
	if !errors.Is(err, auditErr) {
		t.Fatalf("expected fatal audit error %v, got %v", auditErr, err)
	}
	want := []string{
		"users", "rollouts", "telemetry", "certificates", "audit", "retention",
		"users",
		"users", "rollouts", "telemetry", "certificates", "audit",
	}
	if len(ran) != len(want) {
		t.Fatalf("executed steps = %v, want %v", ran, want)
	}
	for i := range want {
		if ran[i] != want[i] {
			t.Fatalf("executed steps = %v, want %v", ran, want)
		}
	}
}
