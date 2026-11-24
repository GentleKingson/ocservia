package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
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
