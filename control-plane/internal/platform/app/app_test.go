package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

func TestBootstrapPasswordPolicyBeforeStartup(t *testing.T) {
	t.Parallel()
	for _, password := range []string{"nA6/X2k", "password", "12345678901234567890", "ocserviapassword"} {
		if err := Run(context.Background(), config.Config{BootstrapLocalAdmin: true, LocalBootstrapPassword: password}, BuildInfo{}, nil, nil, nil); !errors.Is(err, auth.ErrPasswordPolicy) {
			t.Fatalf("bootstrap reached startup before policy rejection: %v", err)
		}
	}
	if err := validateBootstrapPasswords(config.Config{BootstrapLocalAdmin: true, LocalBootstrapPassword: "nA6/X2kZ"}); err != nil {
		t.Fatalf("valid bootstrap password rejected: %v", err)
	}
}

func TestSchedulerIsolatesNonFatalStepErrorsAndTimeouts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ticks := make(chan time.Time)
	auditErr := errors.New("audit hash chain corrupted")
	pass := 0
	work := maintenanceWork{
		stepTimeout: 10 * time.Millisecond,
		users: func(context.Context) error {
			pass++
			if pass == 2 {
				return database.ErrCommitUnknown
			}
			return nil
		},
		rollouts:     func(context.Context) error { return errors.New("rollout boom") },
		telemetry:    func(stepCtx context.Context) error { <-stepCtx.Done(); return stepCtx.Err() },
		certificates: func(context.Context) error { return errors.New("cert boom") },
		audit: func(context.Context) error {
			if pass >= 3 {
				return auditErr
			}
			return nil
		},
		retention: func(context.Context) error { return errors.New("retention boom") },
	}
	done := make(chan error, 1)
	go func() { done <- runScheduler(ctx, ticks, work, 1, logger) }()
	ticks <- time.Now()
	ticks <- time.Now()
	if err := <-done; !errors.Is(err, auditErr) {
		t.Fatalf("expected fatal audit error %v, got %v", auditErr, err)
	}
}
