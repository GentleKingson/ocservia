package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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
			switch pass {
			case 2:
				return &useroperations.EnforcementCleanupError{Err: database.ErrPermission}
			case 3:
				return database.ErrCommitUnknown
			default:
				return nil
			}
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
			if pass >= 4 {
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

	// After the initial pass, exercise isolated cleanup, uncertain commit, and fatal audit.
	ticks <- time.Now()
	ticks <- time.Now()
	ticks <- time.Now()

	err := <-done
	if !errors.Is(err, auditErr) {
		t.Fatalf("expected fatal audit error %v, got %v", auditErr, err)
	}
	want := []string{
		"users", "rollouts", "telemetry", "certificates", "audit", "retention",
		"users", "rollouts", "telemetry", "certificates", "audit", "retention",
		"users",
		"users", "rollouts", "telemetry", "certificates", "audit",
	}
	if !slices.Equal(ran, want) {
		t.Fatalf("executed steps = %v, want %v", ran, want)
	}
}

func TestSchedulerDistinguishesRetryableAndUncertainDatabaseErrors(t *testing.T) {
	cases := []struct {
		name      string
		fail      error
		continues bool
	}{
		{name: "deadlock", fail: database.ErrDeadlock, continues: true},
		{name: "serialization", fail: database.ErrSerialization, continues: true},
		{name: "database unavailable", fail: database.ErrUnavailable},
		{name: "uncertain commit", fail: errors.Join(database.ErrCommitUnknown, database.ErrDeadlock)},
		{name: "unknown user error", fail: errors.New("invalid user state")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ranAudit := false
			work := maintenanceWork{
				users: func(context.Context) error { return tc.fail },
				audit: func(context.Context) error { ranAudit = true; return nil },
			}
			lastSuccess := make(map[string]time.Time)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			err := work.run(t.Context(), 1, logger, lastSuccess)
			if tc.continues {
				if err != nil || !ranAudit || lastSuccess["audit"].IsZero() {
					t.Fatalf("isolated step error = %v, audit ran = %t, last success = %v", err, ranAudit, lastSuccess)
				}
			} else if !errors.Is(err, tc.fail) || ranAudit {
				t.Fatalf("step error = %v, want %v; audit ran = %t", err, tc.fail, ranAudit)
			}
			if !lastSuccess["users"].IsZero() {
				t.Fatal("failed users step was recorded as successful")
			}
		})
	}
}
